<!--
RFC for team feedback — pre-EP draft. This document follows the EGM concepts-brief
structure and stands alone. When this proposal graduates (issue created in
kagent-dev/kagent), rename to EP-[issue#]-skills-hub.md and migrate the content
into design/template.md sections.
-->

# EP-draft: Skills Hub(kagent 技能注册与发布服务 — ToolHive OCI 路线)

* **Author:** jerry-jibu
* **Date:** 2026-11-16
* **状态:** RFC(征求意见)。本 brief 取代 2026-11-16 早前版本的 SkillHub/S3 路线(该路线 Phase 0 冒烟被实证证伪;完整证据保留于「Alternatives Weighed → 已否决备选 1」)。**文中的两个 Blocker 已由同日 Phase 0' 冒烟实证解决,见文末「Phase 0' 冒烟裁决」。**


## Overview(概述)

引入 **ToolHive**(github.com/stacklok/toolhive,Apache-2.0,Go)作为 kagent 控制面之外的技能基础设施,技能内容以 **OCI artifact** 形态发布到任意 OCI registry(kind 集群已有的 `localhost:5001` 或 in-cluster registry),kagent 侧 **零 Go/proto 改动**:

1. **发布**:`thv skill build` 从本地目录构建;管理员源可用 `thv` 的 `git://host/owner/repo@ref#path` 解析器;`thv skill push` 推到 OCI registry。版本 = OCI tag;导入一律 pin **digest**。
2. **目录/元数据**:toolhive-registry-server(独立仓库,Go + PostgreSQL,API-only,OIDC 默认,claim 级 per-registry 可见性,RFC 9728 discovery,审计日志)是候选的浏览/搜索后端;其 "Portal" web UI 与成熟度**未证实**,toolhive-studio 是 Electron 桌面应用,不在 kagent web UI 集成范围。
3. **kagent 集成**:helm/nginx 加 `/skills-hub/` 反向代理路径(纯配置);kagent UI 经既有 `ui/src/appExtensions/` 框架加 widget 调 Hub 目录 API;导入时把选定版本解析为 kagent 原生 `ArtifactSource.oci`(格式 `repo@sha256:<digest>`,CRD 层强制,`go/api/v1alpha3/agenttemplate_types.go:148`),经既有 `UpdateAgentTemplate`(`proto/kagent/api/v1/agent_templates.proto:15`)写入 `AgentTemplateSpec.skills`。
4. **交付**:Substrate 在 worker 侧拉取并只读卷挂载 OCI 镜像(`docs/architecture/configuration-and-compilation.md:328-334`),凭据走 worker 既有 registry 凭据通道,kagent 不向 agent 配置复制凭据、不向 egress 添加 registry 目的地(`go/core/internal/translator/skills.go:98-107` 中 OCI 走 default 分支,不加目的地)。**这同时绕开了上一轮被证伪的 FetchS3 凭据墙与 #3435 golden-actor egress 问题**。上限:每 ActorTemplate 32 卷,扣除 3 个运行时卷后约 29 个不同 skill 镜像(`configuration-and-compilation.md:340-342`,`go/core/internal/substrate/actor_template.go:158-160`)。
5. **共存**:kagent `BUILTIN_SKILLS` 配置下发目录(`ui/src/api/runtimeConfig.ts:15-55`)原样保留,UI 上与 Hub 技能以 provenance 区分;两者运行时交付路径现已**同轨**(都是 digest-pinned OCI 卷挂载)。

### 本轮核实结论(先说三点)

* **零改动承诺在代码层成立,但有两个非代码前置条件。** CRD 正则只接受 digest 引用(`agenttemplate_types.go:148`),编译器 `name.NewDigest` 拒绝 tag(`skills.go:71`),Substrate 卷接线与 32 卷上限检查已工程化(`actor_template.go:149-160`),OCI 不新增 egress(`skills.go:98-107`)。前置条件:(a) **worker 侧对目标 registry 的信任/凭据是集群配置而非 kagent 代码**,且 `imagePullSecrets` 不会自动配置 worker 拉取(`configuration-and-compilation.md:330-334`)——`localhost:5001` 能否被 worker 拉取(insecure/CA)必须实测;(b) **`thv skill build` 产物的镜像布局兼容性未验证**——仓库明文警告 "Build skill images from scratch with just package contents; arbitrary ORAS artifact layouts are not supported"(`configuration-and-compilation.md:357-359`)。这是本轮最大未决风险,与上一轮 S3 匿名假设属于同一类认识论错误,必须冒烟先行。
* **digest 解析是导入 UX 的拱顶石,责任方至今未指派。** `thv push` 产出 tag;spec 只认 digest。解析步骤(widget 调目录 API?widget 直调 registry v2 API?admin 从 `thv` 输出粘贴?)没有 owner。既有先例是 builtin catalog:目录数据直接携带每个版本的 digest-pinned image(`runtimeConfig.ts:22-29,145-161,172`),widget 从不做解析。
* **"claim 级部分共享" 在 v1 不可达。** kagent nginx 显式剥离 identity 头(`helm/kagent/files/nginx.conf:18-38`),widget 经未鉴权代理调 registry API:registry 若开 OIDC 则 widget 全 401;若关 auth 则 widget 可见一切。claim 可见性只保护 registry API 本身,不保护 widget 路径。它是 Phase 3(SSO)的属性,不是 v1 的属性。

## Analysis(多视角评审结论 — 按 lens 分组,含发明性矛盾与 IFR)

严重度标记:Blocker(必须先解决)/ Major(设计内必须回答)/ Nit(记录即可)。所有 file:line 均已在当前工作树验证。

### Skeptical Architect Brainstormer(design_critic)

* **Blocker — ToolHive 产物与 kagent 交付契约的兼容性是未验证的核心假设。** kagent 契约要求:plain image、仅含包内容、materializer 能在挂载点内按 `source.path` 定位并通过包校验(SKILL.md 必须存在于所选路径,`go/core/pkg/agentplugins/materialize.go:342-344`;符号链接/大小/文件类型检查 `:265-299`)。`thv skill build` 的 manifest 版本、artifactType、layer 结构、是否把 skill 包在子目录里,全部未验证;若 thv 产出 ORAS artifact manifest(OCI 1.1 artifactType)或非标准布局,Substrate 卷拉取或 materializer 校验会拒绝——届时 "零改动" 前提与 S3 轮一样崩塌。**教训已在上轮付过学费:外部组件的运行时行为不因 README 而成立,只因冒烟而成立。**
* **Blocker — digest 解析的 owner 未指派,一键导入在当前设计里是虚构。** 见 Overview。三个候选位置:(a) Hub 目录 API 每版本返回 digest(toolhive-registry-server 是否暴露未证实);(b) widget 经代理直调 registry v2 manifest API(读 `Docker-Content-Digest` 头;需处理 CORS 与鉴权);(c) admin 发布流程固化 digest 进目录 JSON。在 owner 定下之前,不得开始 widget 实现。
* **Major — 安全姿态与上轮同构,但 v1 的授权机制比上轮诚实。** v1 无 kagent 侧鉴权:任何能打开 UI 的人可导入任何技能;但发布权限 = registry push 凭据,这是**真实的**访问控制(上轮 SkillHub 的 web 上传在无鉴权时是空集)。materializer 校验(`materialize.go:265-299`)仍是最后防线而非准入机制;发布侧准入(扫描/审核)在 v1 不存在,必须写成政策:"v1 准入 = registry push 权限,机器扫描不存在"。
* **Nit — 上轮的 nginx 20 MiB 上传上限问题随 CLI-only 发布自动消失**(上传不过 kagent 代理,`helm/kagent/values.yaml:494` 不再相关)。这是 pivot 的附带收益,记录在案。

### Codebase Details Brainstormer(codebase_details)

* **Major — builtin catalog 就是 Hub 目录的 v0 参考实现。** `BuiltinSkillsCatalog`(`runtimeConfig.ts:15-46`)已建模:一个镜像 + 多技能 + 每技能多版本(每版本独立 digest-pinned image + label + path),解析器对每个版本强制 `OCI_DIGEST_REF` 校验(`:145-161,172`),注入链是 helm values → ui ConfigMap → `env-config.js`(`helm/kagent/templates/ui-configmap.yaml:22-24`)。Hub 目录若选择 "catalog JSON" 形态,可直接复用此 shape 与解析代码;widget 的导入操作与 skillsCatalog 的 "选择 builtin 版本写 `source.oci`" 是同一操作换数据源(`ui/src/appExtensions/skillsCatalog/` 六个文件为模板)。
* **Major — OCI 引用 512 字符上限**(`skills.go:76-78`,Substrate 限制)。`<registry-host>/<namespace>/<skill>@sha256:64hex` 远小于 512,正常 namespace 约定无碍;唯一注意点:registry 主机名过长 + 深层 namespace 时留余量。
* **Nit — 导入映射规则需写死四条**:skill name 满足 `validateSkillName`(`materialize.go:183+`,单路径组件、小写);`source.path` 取决于 thv 镜像内布局(根目录 vs `skills/<name>/`,Phase 0' 定);digest 导入时刻解析并固化,**永不写 tag**;解析失败必须 fail-closed(拒绝导入),不允许退化成 "先写 tag 再说"。
* **Nit — 既有硬上限不变**:`skills` MaxItems=50(`agenttemplate_types.go:209-213`)、名字唯一(`listMapKey=name` + `materialize.go:169-181` 覆盖 plugin bundle 合并后查重)。29 镜像卷上限比 50 项 skills 上限**更先**触发(多技能共享同一镜像可缓解)。

### Zoom-out Brainstormer(zoom_out)

* **Major — ToolHive 是一个 bundle,本方案只需要其中一个薄片。** 全家桶 = `thv` CLI + K8s operator + registry-server(PostgreSQL + OIDC + 审计)+ studio(Electron)+ keycloak 资产 + MCP gateway。kagent 技能分发需要的全部输入是:**"把一个 skills 目录放进 OCI registry,布局可预测,外加一份可浏览的目录"**。内容存储已由 kind 的 `localhost:5001` 免费提供;目录浏览的最小形态是 catalog JSON(builtin 先例)。registry-server 的差异化能力(OIDC、claim 可见性、审计、跨 namespace 搜索)全部只在多用户 + SSO 阶段才有价值——而那个阶段是 Phase 3。YAGNI 的量化:为 v1 引入 PostgreSQL + OIDC + operator CRD,三项全部闲置。
* **Major — 持久性故事并非自动优于 S3,契约仍要写。** Substrate 在 **restore 时重新准备镜像卷**(`configuration-and-compilation.md:329-330`):registry 是每个使用 Hub 技能的 agent 的**终身依赖**(golden snapshot 之后仍如此)。worker 镜像缓存缓解但不构成契约。digest 是内容寻址(完整性优于 S3 versionId),但 registry GC 未引用 digest、删 repo 杀死 pinned digest、`thv` 重建不保证 bit-identical digest(构建非 hermetic)。必须成文:pin 的 digest 永不 GC、registry 备份策略、或 "重建即新版本需再导入" 的明示语义。
* **Positive — 交付格式终于同轨。** 上轮 S3 路线把仓库标注的弱势分发路径制度化;上轮的 Phase 4 决策点("发布在 Hub,交付在 Substrate")在本次 pivot 中于 v0 直接达成。Hub 技能与 builtin 技能共用一条运行时通道,失败模式统一(卷挂载失败),排查路径单一。这是 pivot 相对 S3 路线的最强改进。
* **Nit — `localhost:5001` 是 kind 的开发工件。** 生产 registry(带 auth、备份、GC 策略)是 Phase 3+ 事项;可以推迟,但必须Named,不能默认 "kind registry 就是生产"。

### Comprehension Brainstormer(comprehension)

* **Major — "零 kagent 改动" 的精确表述应为:"零 Go/proto 改动;helm/UI 配置改动仅限 nginx location、basePath 保留前缀白名单一行(`ui-configmap.yaml:13`)、UI env 注入一项;registry 信任是 Substrate worker / 集群配置,走既有工程化通道,经冒烟验证"。** 把 config-level 触碰诚实计入,避免 "零改动" 被读成 "零仓库触碰"。上轮已实证:配置面(CRD/API/status)对 `ArtifactSource` 全链接受(冒烟 `Accepted`/`Compatible` 全 True),本轮换成 `source.oci` 后该结论沿用且更强(CRD 正则 + `NewDigest` 双重接受)。
* **Major — 验收链必须更新为 OCI 版**,并新增两个上轮没有的用例:thv 发布 → widget 导入(digest 已解析)→ Agent revision Ready(证明 worker 拉取 thv 产物成功)→ **registry 停机下 restore**(记录失败模式,契约写入文档)→ **Hub 元数据层停机下已导入 digest 的 agent 生命周期不受影响**(证明运行时只依赖 registry 不依赖 Hub)。保留回滚测试(pin 旧 digest)。
* **Major — "v1 的 admin" 有了真实定义。** registry push 凭据持有者 = 发布者。这是可验证的授权,不再是上轮的空集概念。文案与文档应这样表述,SSO 阶段再叠加人的角色。
* **Nit — Python ADK 不消费 `agent_plugins` 运行时契约**(`configuration-and-compilation.md:360-362`),Hub 技能对其行为未定义;v1 文档声明 "仅 Go ADK/Codex/Claude 路径已验证"。

### Implementation Readiness Brainstormer(implementation_readiness)

* **Blocker — Phase 0' 冒烟闸门,先于一切 UI/registry-server 工作。** 清单(每项有独立裁决语义):
  1. `thv skill build` 本地 skill → `thv skill push` 到 `localhost:5001` → **检查 manifest**:mediaType 是否标准 image manifest、layer 结构、SKILL.md 是否在可预测路径。对照 `configuration-and-compilation.md:357-359` 的布局要求。
  2. kagent 导入该镜像的 `repo@sha256:…` → Agent revision Ready(覆盖 worker 从 kind 节点拉取 `localhost:5001` 的可达性 + 信任配置;这是 Overview 前置条件 (a) 的实证)。
  3. registry 停机 → 对已 pin 该 digest 的 actor 执行 restore → 记录行为(契约素材)。
  4. (可选,与 1-3 解耦)toolhive-registry-server 部署验证——**仅当走 registry-server 路线**才做。
  **裁决规则与上轮一致:1 或 2 失败 → 方案重估,不是修补。**
* **Major — toolhive 源码核查清单(Phase 0' 的案头部分,开源可读,成本远低于运行时意外):** manifest/artifactType 生成逻辑;push 输出是否含 digest(影响 UX);skill build 的目录布局约定(影响 `source.path`);registry-server 目录 API 是否暴露每版本 digest(影响 Blocker 2 的 owner 归属);registry-server 是否存 artifact 还是仅元数据(仅元数据则内容路径完全不经过它,强 IFR)。
* **Major — operator vs 纯部署是 registry-server 内部的独立决策。** 若 v1 只需 API-only catalog,纯 Deployment(+ PostgreSQL)可能足够;operator + CRD 是多副本/声明式运维的收益,单实例 kind 场景是死重。不要因为上游 README 主推 operator 就照单全收。
* **Nit — UI env 注入点仍需一项**(Hub 目录地址,沿 `KAGENT_UI_BUILTIN_SKILLS` 同链路,`ui-configmap.yaml:22-24` 为模板),列入工作量,虽然小。

### Structural Brainstormer(structural)

* **Positive — 结构耦合面与上轮同宽且更干净。** kagent 侧唯一契约是已存在的 `source.oci` 线格式;widget 代码收纳在 `ui/src/appExtensions/skillsHub/`;registry-server(若采用)以自带 chart 独立安装,**不得**成为 `helm/kagent` 的依赖子 chart,否则 "独立" 名存实亡。
* **Major — 同轨交付改变 UI 叙事策略。** builtin 与 Hub 现在共享运行时通道,区别只剩 "平台精选 vs 用户发布"。建议不把区别做进数据结构(`AgentTemplateSkill` 只有 name+source,`agenttemplate_types.go:98-104`,无处记录 provenance,也不要为此加字段);provenance 作为 UI 展示态(widget 导入时记录来源)即可,未来可平滑合并为单一列表 + 来源徽章。
* **Nit — basePath 保留前缀**:`/skills-hub` 加入 `ui-configmap.yaml:13` 的 fail 正则枚举是一行模板改动;SPA fallback 需确认新 location 显式优先于 index.html 回落。

### Inventive Catalyst Brainstormer(inventive_catalyst)

* **矛盾 1 → IFR:"谁是事实源?" registry(digest)必须是,Hub 元数据必须可从 registry 重建。** IFR:**"删掉 Hub 的元数据存储,目录可从 registry 的 tag 列表 + artifact 内嵌元数据重建。"** 若 thv 把 skill 元数据写进 OCI annotations(未验证,Phase 0' 案头清单),registry-server 的 PostgreSQL 退化为搜索缓存;若不写,一个 git-backed 的 catalog JSON 侧车即可。此矛盾同时化解持久性问题的大半:已 pin 的 digest 与新 actor 拉取永远只依赖 registry。
* **矛盾 2 → registry-server 现在上还是永远不上。** 分辨率:**v1 不上**。理由的三重合一:内容路径完全不经过它(OCI registry 直连 worker);其差异化能力(OIDC/claim/审计/搜索)在 v1 全部闲置(v1 无 SSO,widget 代理剥离 identity 头);catalog JSON 可覆盖 v1 规模的浏览/搜索(用户群 ≤ 数十技能时 widget 客户端过滤足矣)。**在 Phase 3(SSO/multi-user)引入它,彼时 OIDC、claim 可见性、审计同时变得有用**——那时它不是死重而是正好。
* **矛盾 3 → tag 便利性 vs digest 安全性。** 分辨率:导入时解析、fail-closed、**digest 即用户可见的版本身份**(builtin catalog 的 label 先例:`runtimeConfig.ts:36-38`)。信任性论证:运行时按 digest 拉取,不存在 tag 的 TOCTOU;唯一的信任时刻是导入瞬间的 tag→digest 解析,应记录解析结果(来源 + 时间),使其可审计。
* **矛盾 4 → CLI-only 发布对谁是真的损失?** 分辨率:对当前用户群(管理员发布、终端用户消费)不是损失——CLI + registry push 凭据就是发布授权,且可挂 CI。web 上传有价值的前提是非管理员用户生产内容,那与鉴权同阶段(Phase 3)。**此 pivot 的取舍成立当且仅当 "管理员发布" 是真实用户模型——写成显式假设,用户群变化时重审。**
* **发明 — IFR 全文:"技能从源码流向 agent 运行时,kagent 零改动,导入时刻之后 Hub 元数据层永久退场。"** OCI 路线达成了上轮 S3 路线未达成的形态:上轮 import 后运行时仍依赖 bucket + FetchS3(且 FetchS3 本身崩溃);本轮 import 后运行时只依赖 registry + 既有 worker 通道。差异:Hub(元数据)可彻底退场,registry(内容)**不可**退场但走工程化通道。"import once, runtime independent" 对 **Hub** 成立,对 **registry** 不成立(restore 仍需卷准备,`configuration-and-compilation.md:329-330`)——这个精确边界必须写进契约,不得含糊为 "永远独立"。

### 最小可行子集(contrarian 裁决)

**`thv` CLI(仅 build/push)+ 既有/plain OCI registry + Hub catalog JSON + widget 导入。** 从假设中删除:registry-server(PostgreSQL/OIDC/审计)、operator + CRD、studio(Electron 形态错误;Portal 未证实)、keycloak 资产。v1 四件套全部有既有仓库先例背书(catalog JSON = `runtimeConfig.ts` shape;widget = `skillsCatalog` 扩展;导入 = builtin picker 同操作;交付 = OCI 卷挂载)。registry-server 作为 Phase 3 的第一候选重新评估,非否决。

## Goals vs. Non-Goals(目标与非目标)

### Goals(v1)

1. kagent Go/proto 零改动;helm/UI 触碰限于:nginx `/skills-hub/` location(values 开关)、basePath fail-list 一行、UI env 一项。
2. 发布:`thv skill build/push`(管理员,凭据 = registry push 权限;管理员 git 源走 `git://` 解析器)。无 web 上传。
3. 目录与导入:widget 浏览/搜索/导入;导入输出 `ArtifactSource.oci{repo@sha256:<digest>}`(± `source.path`),digest 导入时解析、fail-closed、永不写 tag。
4. Phase 0' 冒烟全绿:thv 布局兼容、worker 拉取 `localhost:5001`、registry 停机 restore 行为记录在案。
5. 持久性契约成文:pin 的 digest 不 GC、registry 可用性是 agent 终身依赖(含 restore)、Hub 元数据可从 registry 重建(IFR)。
6. BUILTIN_SKILLS 目录保留;UI provenance 区分(展示层,非数据层)。
7. E2E 验收链全绿(见 Comprehension lens,含停 Hub 元数据层不受影响用例)。

### Non-Goals(v1 明确不做)

1. **toolhive-registry-server / operator / studio / keycloak 资产的部署**——Phase 3 候选,非 v1 组件(除非 Phase 0' 推翻 catalog JSON 路线)。
2. SSO/OIDC 统一、claim 级可见性、审计(Phase 3;届时 nginx identity 剥离清单 `nginx.conf:18-38` 必须重审)。
3. web 上传、fork/modify、审批流、多用户组织。
4. kagent CRD/API/proto 任何改动;provenance 的 CRD 字段(违反零改动前提,永不加)。
5. Python ADK 的 Hub 技能支持声明。
6. "运行时永久独立于 registry" 的承诺——restore 需要卷准备,registry 是终身依赖,只承诺契约化管理。

## Alternatives Weighed(已权衡的备选及否决理由 — 含 SkillHub/S3 路线的证伪记录)

### 已否决备选 1:SkillHub(iflytek)+ S3 交付路线(上轮选定,本轮被证伪否决)

**证伪记录(2026-11-16,kind 集群端到端实证,原样保留):**

* **实测通过**:RustFS 桶 `skills-hub-smoke` + 版本控制 + 匿名读 policy + zip 对象 versionId `b335a5b7-1128-4230-a8df-c7a182b009c0`;netpol 补放行后 ClusterIP 可达;busybox 无凭据无 Authorization 头成功取回 zip(`PK` 魔数);`ArtifactSource.s3` 配置面全链 `Accepted`/`Compatible`——**配置面零后端改动成立**。
* **实测失败(裁决点)**:agent 端到端构建失败于运行时取件层,worker 日志原文:`materialize skill "smoke-skill": get s3://skills-hub-smoke/skills/smoke-skill/1.0.0.zip: operation error S3: GetObject, exceeded maximum number of attempts, 3, get identity: get credentials: failed to refresh cached credentials, no EC2 IMDS role found, ... dial tcp 169.254.169.254:80: connect: no route to host`。链条:`FetchS3` 走 AWS SDK 默认凭据链(`go/core/internal/skillsinit/s3.go:32,42-57`,无 `AnonymousCredentials` 回退)→ 沙箱内 IMDS 不可达 → SDK 客户端拒绝发无签名请求 → 进程启动即退 → wakeup probe 永不 200 → `GoldenActorCrashed`。对照组:裸 HTTP 匿名取件成功(服务端放行无问题),aws-cli 无凭据同失败(客户端拒绝)。**失败在客户端凭据链,与桶策略无关;`versionId requires an S3 archive object`(`s3.go:71-73`)行为符合预期。**
* **否决理由**:修复三选一(匿名回退 Go 补丁 / #2815 凭据支持 / 提前转 OCI 导出)中,(a) 违反零改动前提,(b) 依赖上游进度,(c) 就是本轮路线——且 (c) 直接消灭 FetchS3 凭据墙与 #3435 egress 两个暴露面。S3 路线整体降级,不再作为主方案。
* **遗留资产**:`skills-hub-smoke` 桶与 netpol 放行规则保留;S3 路线的其余设计资产(一版本一 zip、versionId 固化、bucket 契约)随路线否决一并作废。

### 已权衡备选(本轮)

2. **ToolHive 全家桶(thv + registry-server + operator + studio)按假设原样采纳。** 部分否决:仅采纳 `thv` CLI;registry-server/operator/studio/keycloak 降为 Phase 3 候选(v1 全部闲置,见 zoom-out 与矛盾 2)。
3. **最小可行子集(选定):thv + plain registry + catalog JSON + widget。** 理由:四件套全部有仓库先例;registry-server 的差异化能力与 v1 无交集;内容路径不经过它。
4. **SkillHub(保留 web UI/RBAC/web 上传的原始价值主张)。** 否决(v1):核心交付撞已证伪的 FetchS3 凭据墙;其差异化能力(web UI、RBAC、web 上传)对当前用户群(admin 发布)价值有限,且 web 上传的价值前提(非管理员生产内容)与 SSO 同阶段。若 Phase 3 需要 web 化多用户注册服务,届时与 registry-server 同台重比。
5. **自研极小 Go 服务 / 无新服务(CI 生成目录镜像走 BUILTIN_SKILLS 链)。** 否决:自研服务在 catalog JSON 已能覆盖浏览/搜索时是纯增项;"无新服务" 路线(即现状)没有用户自发布入口,不满足核心诉求——但注意最小子集与它的距离只剩 "catalog JSON 的来源是 Hub 而非 kagent values"。
6. **kagent 原生 Hub(在 kagent 控制面内做技能注册)。** 否决:违反组件边界(store 不发资产、API 不描述基础设施机制),且与零改动前提矛盾。

## Key Decisions & Staging for Next Steps(关键决策与下一步阶段)

### 关键决策(评审后存活)

1. **零改动承诺升级为三段式精确表述**:Go/proto 零改动(代码已验证)+ helm/UI config 最小触碰(列明三项)+ 集群/worker 侧 registry 信任(既有工程化通道,冒烟实证)。任一段失败回到 Phase 0' 闸门。
2. **v1 组件 = thv + plain OCI registry + catalog JSON + widget**;registry-server 及其余 ToolHive 组件推迟到 Phase 3 重新评估。catalog JSON 的宿主与更新机制(CI 从 git 生成 / thv post-push hook / 手工)在 Phase 1 定。
3. **digest 是唯一 pin 形态**:导入时解析、fail-closed、digest 即 UI 版本身;解析动作记录来源与时间(可审计)。
4. **发布授权 = registry push 凭据**;v1 无发布侧扫描/审核,写成明示政策(机器闸门不存在是决策,不是疏忽)。
5. **registry 终身依赖契约成文**:digest 不 GC、restore 需卷准备、备份策略、"Hub 元数据可从 registry 重建" 作为 IFR 验收标准。
6. **用户模型假设显式化**:"管理员发布、终端用户消费" 是本方案的成立前提;非管理员生产内容的需求出现时触发 Phase 3 重审(web 上传 + 鉴权 + registry-server)。

### 阶段划分

* **Phase 0' — 冒烟闸门(先于一切实现)**:(1) thv build/push → manifest 布局检查(对照 `configuration-and-compilation.md:357-359`);(2) kagent 导入 digest → actor Ready(worker 拉取实证);(3) registry 停机 restore 行为记录;(4) toolhive 源码案头核查(manifest 生成、push digest 输出、目录 API 是否暴露 digest、artifact 存储与否)。1 或 2 失败 → 方案重估。
* **Phase 1 — 只读集成**:catalog JSON 供给机制;nginx `/skills-hub/` location + basePath fail-list + UI env;widget 只读(浏览/搜索/详情)。
* **Phase 2 — 导入链路与 E2E**:tag→digest 解析组件(含 fail-closed 与审计记录);导入映射(name/path/digest);E2E 验收链(含停 Hub 元数据层、回滚旧 digest、registry 停机 restore 记录)。
* **Phase 3 — SSO/多用户**:OIDC 统一;registry-server 重评(claim 可见性此时才可用);web 上传需求重审;nginx identity 剥离清单重审。
* **Phase 4 — 生产 registry 化**:真实 registry(auth、备份、GC 策略)替换 kind `localhost:5001`;digest 保留契约落地为 registry 配置。

### 遗留观察(不阻塞,记录在案)

* `thv skill build` 在开发者非 Linux 机器上的可用性未验证(影响管理员发布体验)。
* 多技能共享同一镜像可推迟触碰 29 镜像卷上限;Hub 若未来推 "一技能一镜像" 惯例,上限从 50(skills 项)实际收缩为 29(镜像数),交互设计需在接近上限时提示。
* standalone 技能与 plugin bundle 技能同名在运行时 materialize 报错(`materialize.go:169-181`),导入前置检查应覆盖 plugin 来源名字。
* catalog JSON 若经 nginx 代理下发,注意 SPA fallback(`nginx.conf:187-189` 一带)不得把 API 路径回落成 index.html 200。

## Phase 0' 冒烟裁决(2026-11-16 实证):**全部通过,路线成立**

两个 Blocker 在 kind 集群端到端实证,与 S3 轮的证伪形成对照:

### Blocker 1(镜像布局兼容性)→ 通过

`thv skill build`(本地目录 → OCI artifact)+ `thv skill push --no-sign`(→ `localhost:5001`)产出经 registry API 逐层核验:

| 项 | 实测 |
| --- | --- |
| manifest 形态 | 多架构 OCI index(`dev.toolhive.skills.v1`,amd64+arm64) |
| config | **标准** `application/vnd.oci.image.config.v1+json`(非 ORAS 空壳),技能元数据在 Labels |
| layer | 标准 `tar+gzip`,**SKILL.md 在层根**(`SKILL.md`、`run.sh`) |
| kagent 挂载 | Substrate 以 image volume 挂载:worker 日志实测 `image_volume_mounts:[{"name":"skill-0","mount_path":"/run/kagent/artifacts/d64dd1ea…"}]` —— 即 `SkillImage.MountPath()` 编译产物 |

`thv` 的布局恰好落在"标准 image"一侧,未触碰"arbitrary ORAS artifact layouts"禁区。

### Blocker 2(digest 解析归属)→ 通过,归属确立

registry `HEAD/GET manifests` 返回 `Docker-Content-Digest`(index digest `sha256:f3f6184a…` 与 per-arch digest `sha256:1b7e984d…`)。**归属裁定:widget 导入时经 registry API 一次 HEAD 完成 tag→digest 解析并固化 per-arch digest**,无需 toolhive 参与。

### 端到端结果

```
thv skill build /tmp/smoke-skill --tag localhost:5001/skills/smoke-skill:v1
thv skill push --no-sign localhost:5001/skills/smoke-skill:v1
AgentTemplate.spec.skills[0].source.oci = localhost:5001/skills/smoke-skill@sha256:1b7e984d…
→ Accepted / EgressPoliciesApplied / ResolvedRefs / Compatible / **Ready=True**
→ actor checkpoint 完成(快照含技能卷)
```

与 S3 轮 `GoldenActorCrashed` 形成直接对照。**零 Go/proto 改动承诺在运行时层全面证实**;发现的两个部署层注意点已补录:
1. `thv` CLI 是 `thv serve` 的瘦客户端(build/push 走 `/api/v1beta/skills`);本地 registry 需服务端 `TOOLHIVE_DEV=true` 启用 loopback plain-HTTP(生产走 TLS registry 无此问题);
2. `push` 默认要求签名(`--no-sign` 可关);企业内可配 signing credential 作为治理面而非阻碍。

### 残留待办(不阻塞概念,归入后续阶段)

- chat 端 `load_skill` 的真实调用验证(需真实 model key;Ready 已证明加载面);
- digest 保留契约(registry GC 策略)书面化;
- widget 导入交互中 tag→digest 解析的实现(归 Phase 2)。

冒烟产物保留:`skills/smoke-skill:v1`(registry)、`oci-smoke-template` / `oci-smoke-agent`(kagent,供 UI 手测)。
