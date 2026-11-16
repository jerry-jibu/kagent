import { fireEvent, render, screen } from "@testing-library/react";
import { ThemeProvider } from "@emotion/react";
import { SWRConfig } from "swr";
import { afterEach, expect, it, vi } from "vitest";
import { Code, ConnectError } from "@connectrpc/connect";
import type { Transport } from "@connectrpc/connect";
import { apiClient } from "@/api/client";
import { setApiTransport } from "@/api/transport";
import { mockTransport } from "@/mocks/transport";
import { themeFor } from "@/theme/theme";
import { mockBuiltinSkills } from "@/mocks/fixtures";
import type { CatalogSelection } from "@/components/skills/catalogSelection";
import { AgentSkillsDialog } from "./AgentSkillsDialog";

afterEach(() => {
  setApiTransport(undefined);
  delete window.environmentVariables;
  vi.restoreAllMocks();
});

/**
 * The dialog reads through the same mock backend the browser mock mode uses, so
 * its writes are real against the fixtures — each case saves and then reads back
 * over the API. The catalog rides the environment exactly as env-config.js does.
 */
function startBackend(catalog: string) {
  window.environmentVariables = { BUILTIN_SKILLS: catalog };
  setApiTransport(mockTransport);
}

/** Opens an antd Select by its test id and clicks the option whose content reads `option`. */
async function chooseOption(testId: string, option: string) {
  fireEvent.mouseDown(screen.getByTestId(testId));
  const content = await screen.findByText(option, { selector: ".ant-select-item-option-content" });
  fireEvent.click(content.closest(".ant-select-item-option")!);
}

function renderDialog(initialAdd: CatalogSelection[] = []) {
  render(
    <ThemeProvider theme={themeFor("dark")}>
      <SWRConfig value={{ provider: () => new Map(), shouldRetryOnError: false }}>
        <AgentSkillsDialog open initialAdd={initialAdd} onClose={() => {}} />
      </SWRConfig>
    </ThemeProvider>,
  );
}

const atOlderVersion = (name: string): CatalogSelection => {
  const skill = mockBuiltinSkills.skills.find((entry) => entry.name === name)!;
  return {
    name,
    description: skill.description,
    image: skill.versions[0]?.image ?? mockBuiltinSkills.image,
    path: skill.versions[0]?.path ?? skill.path,
    tags: skill.tags,
  };
};

it("Given an inline-template agent, when saving two catalog selections, assert that the agent spec carries each skill at its chosen version", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  renderDialog([atOlderVersion("k8s-troubleshooting"), atOlderVersion("prometheus-observability")]);

  await chooseOption("agent-skills-agent-picker", "kagent/release-notes");
  expect(await screen.findByText("Inline template")).toBeDefined();
  fireEvent.click(screen.getByTestId("agent-skills-save"));

  const agent = await apiClient.agentBuildingBlocks.agent("kagent", "release-notes");
  const skills = agent.resource.spec.template.inline?.skills ?? [];
  expect(skills.map((skill) => skill.name).sort()).toEqual(["k8s-troubleshooting", "prometheus-observability"]);
  const k8s = skills.find((skill) => skill.name === "k8s-troubleshooting");
  expect(k8s?.source.oci).toBe(mockBuiltinSkills.skills[3].versions[0].image);
});

it("Given an agent referencing a template other agents share, when adding a skill, assert that the shared-template warning shows and the template gains the skill", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  renderDialog([atOlderVersion("helm-release-management")]);

  await chooseOption("agent-skills-agent-picker", "kagent/shared-brain-fast");
  expect((await screen.findByTestId("agent-skills-shared-template")).textContent).toContain("shared by 3 agents");
  fireEvent.click(await screen.findByTestId("agent-skills-save", {}, { timeout: 5000 }));

  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  expect(template.resource.spec.skills?.map((skill) => skill.name)).toContain("helm-release-management");
});

it("Given a template holding catalog and custom skills, when a pre-checked skill is unchecked, assert that only it is removed and held pins are left alone", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  renderDialog([]);

  await chooseOption("agent-skills-agent-picker", "kagent/k8s-agent-7f3a91c");
  // The browser opens with the template's catalog skills checked. The Git skill
  // is not the catalog's, so it renders no row at all — the catalog cannot
  // manage what it did not provide, and the save must leave it alone.
  const k8sCheck = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(k8sCheck).toHaveProperty("checked", true);
  expect(screen.getByTestId("skills-catalog-check-helm-release-management")).toHaveProperty("checked", true);
  expect(screen.queryByTestId("skills-catalog-card-incident-review")).toBeNull();
  // helm-release-management sits on an older kagent-tools digest than this
  // catalog pins, so the row says so instead of planning a silent upgrade.
  expect(
    screen.getByTestId("skills-catalog-outdated-helm-release-management").textContent,
  ).toContain("Older version");

  fireEvent.click(screen.getByTestId("skills-catalog-check-k8s-troubleshooting"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  const skills = template.resource.spec.skills ?? [];
  const names = skills.map((skill) => skill.name);
  expect(names).not.toContain("k8s-troubleshooting");
  expect(names).toContain("incident-review");
  expect(names).toContain("helm-release-management");
  // Untouched, helm-release-management keeps the digest it was saved with: the
  // removal must not upgrade every held row to the catalog's current pin.
  const helm = skills.find((skill) => skill.name === "helm-release-management");
  expect(helm?.source.oci).toBe(
    "ghcr.io/kagent-dev/kagent/tools@sha256:1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
  );
});

it("Given no catalog configured, when an agent is chosen, assert that the dialog says there is nothing to add", async () => {
  startBackend("");
  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/release-notes");
  expect(
    await screen.findByText(/No skills catalog is configured/),
  ).toBeDefined();
});

it("Given two agents sharing a template, when the picker switches agents mid-manage, assert that each save writes only its own target", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  // Seed: the shared template holds two catalog skills, so all three agents on
  // it hold the same skills. (Tests above in this file rewrite their templates,
  // so every case here seeds what it stands on.)
  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  const at = (name: string) => ({
    name,
    source: { oci: mockBuiltinSkills.image, path: `opt/kagent-tools-skills/skills/${name}` },
  });
  await apiClient.agentBuildingBlocks.updateAgentTemplate({
    namespace: "kagent",
    name: "shared-brain",
    resource: {
      metadata: template.resource.metadata,
      spec: { ...template.resource.spec, skills: [at("k8s-troubleshooting"), at("prometheus-observability")] },
    },
  });

  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/shared-brain");
  const check = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(check).toHaveProperty("checked", true);
  // A removal is staged against shared-brain...
  fireEvent.click(check);

  // ...then the picker moves to another agent on the same template. The browser
  // must remount for the new target: a staged removal belongs to the agent it
  // was made against, and carrying it over would write it into the next save.
  await chooseOption("agent-skills-agent-picker", "kagent/shared-brain-fast");
  const remounted = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(remounted).toHaveProperty("checked", true);

  // The fresh view stages nothing, so its Save stays dark: the removal staged
  // above cannot leak into a save here, and nothing writes the shared template.
  expect(await screen.findByTestId("skills-catalog-apply", {}, { timeout: 5000 })).toHaveProperty(
    "disabled",
    true,
  );
  const afterB = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  expect(afterB.resource.spec.skills?.map((skill) => skill.name).sort()).toEqual([
    "k8s-troubleshooting",
    "prometheus-observability",
  ]);

  // Back on the first agent the staged removal is gone with the remount, so it
  // is staged again — and this save is the one that removes the skill.
  await chooseOption("agent-skills-agent-picker", "kagent/shared-brain");
  const staged = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(staged).toHaveProperty("checked", true);
  fireEvent.click(staged);
  fireEvent.click(await screen.findByTestId("skills-catalog-apply", {}, { timeout: 5000 }));

  const afterA = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  expect(afterA.resource.spec.skills?.map((skill) => skill.name)).toEqual(["prometheus-observability"]);
});

it("Given a template holding exactly one catalog skill, when it is unchecked, assert that Save comes alive and the save removes it", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  // Seed a target whose only held catalog skill is k8s-troubleshooting, so the
  // removal leaves nothing checked at all. (Tests above rewrite their
  // templates, so this case seeds what it stands on.)
  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  await apiClient.agentBuildingBlocks.updateAgentTemplate({
    namespace: "kagent",
    name: "shared-brain",
    resource: {
      metadata: template.resource.metadata,
      spec: {
        ...template.resource.spec,
        skills: [
          {
            name: "k8s-troubleshooting",
            source: { oci: mockBuiltinSkills.image, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting" },
          },
        ],
      },
    },
  });

  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/shared-brain");
  const check = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(check).toHaveProperty("checked", true);
  // Held rows alone stage nothing, so Save is dark until something changes.
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", true);

  // Unchecking the target's only held catalog skill is a removal, and must be
  // savable even though nothing stays checked.
  fireEvent.click(check);
  const save = await screen.findByTestId("skills-catalog-apply", {}, { timeout: 5000 });
  expect(save).toHaveProperty("disabled", false);
  fireEvent.click(save);

  const after = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "shared-brain");
  expect(after.resource.spec.skills ?? []).toEqual([]);
});

it("Given an agent whose referenced template no longer exists, when the dialog resolves the target, assert that the read failure is named instead of spinning forever", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  await apiClient.agentBuildingBlocks.createAgent({
    namespace: "kagent",
    name: "dangling-template-ref",
    resource: {
      metadata: { name: "dangling-template-ref", namespace: "kagent" },
      spec: {
        template: { ref: { name: "deleted-template" } },
        harnessRef: { name: "k8s-agent" },
      },
    },
  });

  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/dangling-template-ref");
  const failure = await screen.findByTestId("agent-skills-template-error", {}, { timeout: 5000 });
  expect(failure.textContent).toContain("Could not read the template deleted-template");
  // Nothing to spin on and nothing to save: every write below would go through
  // the template this dialog cannot read.
  expect(screen.queryByTestId("agent-skills-save")).toBeNull();
});

it("Given a template write that is refused, when the save fails and is retried, assert that the selection survives the failure and the retry succeeds", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  // Seed the template the way the fixture has it, so this case stands on its own
  // write whatever the tests above it saved.
  const seeded = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  await apiClient.agentBuildingBlocks.updateAgentTemplate({
    namespace: "kagent",
    name: "k8s-agent-7f3a91c",
    resource: { metadata: seeded.resource.metadata, spec: { ...seeded.resource.spec, skills: [
      { name: "k8s-troubleshooting", source: { oci: mockBuiltinSkills.image, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting" } },
      { name: "helm-release-management", source: { oci: "ghcr.io/kagent-dev/kagent/tools@sha256:1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809", path: "opt/kagent-tools-skills/skills/helm-release-management" } },
      { name: "incident-review", source: { git: { url: "https://github.com/example/runbooks.git", commit: "3f1c9d2e5b7a48e0a1c6d9f2b4e8a7c30d5f6e9b" }, path: "skills/incident-review" } },
    ] } },
  });

  // Refuse exactly one template write, then let the mock backend answer.
  let refused = false;
  setApiTransport({
    stream: (method, signal, timeoutMs, header, input, contextValues) =>
      mockTransport.stream(method, signal, timeoutMs, header, input, contextValues!),
    unary: (method, signal, timeoutMs, header, input, contextValues) => {
      if (
        !refused &&
        method.parent.typeName === "kagent.api.v1.AgentTemplateService" &&
        method.name === "UpdateAgentTemplate"
      ) {
        refused = true;
        return Promise.reject(new ConnectError("the template write was refused", Code.Aborted));
      }
      return mockTransport.unary(method, signal, timeoutMs, header, input, contextValues!);
    },
  } as Transport);

  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/k8s-agent-7f3a91c");
  const check = await screen.findByTestId("skills-catalog-check-k8s-troubleshooting", {}, { timeout: 5000 });
  expect(check).toHaveProperty("checked", true);
  fireEvent.click(check);
  fireEvent.click(await screen.findByTestId("skills-catalog-apply", {}, { timeout: 5000 }));

  const alert = await screen.findByText("Save failed", {}, { timeout: 5000 });
  expect(alert).toBeDefined();
  // The browser kept the failed selection instead of resetting: the removal is
  // still staged and the other held rows still checked.
  expect(screen.getByTestId("skills-catalog-check-k8s-troubleshooting")).toHaveProperty("checked", false);
  expect(screen.getByTestId("skills-catalog-check-helm-release-management")).toHaveProperty("checked", true);

  fireEvent.click(await screen.findByTestId("skills-catalog-apply", {}, { timeout: 5000 }));
  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  const skills = template.resource.spec.skills ?? [];
  const names = skills.map((skill) => skill.name);
  expect(names).not.toContain("k8s-troubleshooting");
  expect(names).toContain("helm-release-management");
  expect(names).toContain("incident-review");
  // The untouched held row kept its older digest through both writes.
  expect(skills.find((skill) => skill.name === "helm-release-management")?.source.oci).toBe(
    "ghcr.io/kagent-dev/kagent/tools@sha256:1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
  );
});

it("Given an add whose name the target holds from another source, when the dialog opens, assert that the collision is named and the held skill survives the save", async () => {
  startBackend(JSON.stringify(mockBuiltinSkills));
  // The template holds a Git-sourced skill under a catalog name: the catalog
  // cannot manage it, so an add of that name must be skipped, not merged over it.
  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  const foreign = {
    name: "k8s-troubleshooting",
    source: { git: { url: "https://git.test/runbooks.git", commit: "c".repeat(40) } },
  };
  await apiClient.agentBuildingBlocks.updateAgentTemplate({
    namespace: "kagent",
    name: "k8s-agent-7f3a91c",
    resource: { metadata: template.resource.metadata, spec: { ...template.resource.spec, skills: [foreign] } },
  });

  renderDialog([atOlderVersion("k8s-troubleshooting")]);
  await chooseOption("agent-skills-agent-picker", "kagent/k8s-agent-7f3a91c");
  const skipped = await screen.findByTestId("agent-skills-skipped", {}, { timeout: 5000 });
  expect(skipped.textContent).toContain("1 selection");
  expect(skipped.textContent).toContain("k8s-troubleshooting");

  fireEvent.click(await screen.findByTestId("agent-skills-save", {}, { timeout: 5000 }));
  const after = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  const heldSkill = after.resource.spec.skills?.find((skill) => skill.name === "k8s-troubleshooting");
  expect(heldSkill?.source.git).toBeDefined();
  expect(after.resource.spec.skills).toHaveLength(1);
});
