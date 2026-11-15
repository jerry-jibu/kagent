import { useMemo, useState } from "react";
import { Alert, Button, Modal, Select, Space, Spin, Typography } from "antd";
import { useTheme } from "@emotion/react";
import toast from "react-hot-toast";
import type { Agent } from "@/api/domain/agents";
import type { AgentTemplateSkill } from "@/api/domain/agentTemplates";
import { apiClient } from "@/api/client";
import { useAgent, useAgentsAcrossNamespaces, useInvalidateAgents } from "@/api/hooks/useAgents";
import { useAgentTemplate } from "@/api/hooks/useAgentBuildingBlocks";
import { useInvalidateAgentTemplates } from "@/api/hooks/useInvalidateAgentTemplates";
import { useNamespaces } from "@/api/hooks/useNamespaces";
import { runtimeConfig } from "@/api/runtimeConfig";
import { SkillsCatalogBrowser } from "@/components/skills/SkillsCatalogBrowser";
import {
  applySkillSelections,
  catalogSkillFor,
  type CatalogSelection,
} from "@/components/skills/catalogSelection";

const { Text, Paragraph } = Typography;

/** An agent as the picker addresses it: `namespace/name`, resolved back by split. */
function agentKey(agent: Agent): string {
  return `${agent.namespace}/${agent.name}`;
}

/**
 * Add catalog skills to an agent, or manage the ones it has.
 *
 * One dialog, two entries: "add" arrives from the catalog page carrying
 * selections to place; "manage" starts empty and opens the browser pre-checked
 * with the agent's current catalog skills, so unchecking is the removal. Both
 * resolve to the same two write shapes — an inline template is rewritten on the
 * Agent, a referenced template on the AgentTemplate it names, with a confirm
 * when that template is shared.
 */
export function AgentSkillsDialog({
  open,
  initialAdd = [],
  onClose,
}: {
  open: boolean;
  /** Selections from the catalog page's "Add to agent…" apply. */
  initialAdd?: CatalogSelection[];
  onClose: () => void;
}) {
  const theme = useTheme();
  const catalog = useMemo(() => runtimeConfig().builtinSkills, [open]);
  const namespaces = useNamespaces();
  const agentsResource = useAgentsAcrossNamespaces(namespaces.data?.map((entry) => entry.name));
  const invalidateAgents = useInvalidateAgents();
  const invalidateTemplates = useInvalidateAgentTemplates();

  const [agentKeyChoice, setAgentKeyChoice] = useState<string>();
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState<string>();

  const agents = agentsResource.data?.agents ?? [];
  // The namespace read comes first and the agents read hangs off it, so either
  // failing leaves the dialog without a list — and a failed read must not read
  // as "no agents exist".
  const readFailure = agentsResource.error ?? namespaces.error;
  const listedAgent = agents.find((agent) => agentKey(agent) === agentKeyChoice);
  // Manage mode re-reads the Agent so a dialog opened before a save edits what
  // is really stored; add mode can work from the list row it was given.
  const liveAgent = useAgent(open ? listedAgent?.namespace : undefined, open ? listedAgent?.name : undefined);
  const agent = liveAgent.data ?? listedAgent;

  const referenced = agent?.resource.spec.template.ref?.name;
  const template = useAgentTemplate(
    agent && referenced ? agent.namespace : undefined,
    agent && referenced ? referenced : undefined,
  );

  const effectiveSkills: AgentTemplateSkill[] = referenced
    ? template.data?.resource.spec.skills ?? []
    : agent?.resource.spec.template.inline?.skills ?? [];
  const heldCatalogSkills = useMemo(
    () => (catalog ? effectiveSkills.map((skill) => catalogSkillFor(skill, catalog)) : []),
    [catalog, effectiveSkills],
  );
  const preChecked = heldCatalogSkills.filter((skill) => skill !== undefined).map((skill) => skill.name);
  const unmanaged = effectiveSkills
    .filter((skill) => catalog && catalogSkillFor(skill, catalog) === undefined)
    .map((skill) => skill.name);
  // A referenced template's skills arrive in a second read after the agent's.
  // Every interaction below — pre-checked rows, Save — must wait for them, or
  // the dialog would offer to save against a spec it has not seen.
  const targetReady = !referenced || template.data !== undefined;

  // Sharing is computed from the loaded list, which is the same list the page
  // rendered — the confirm names that count honestly.
  const sharedWith = useMemo(
    () =>
      agent && referenced
        ? agents.filter((entry) => entry.resource.spec.template.ref?.name === referenced && entry.namespace === agent.namespace)
        : [],
    [agents, agent, referenced],
  );

  const close = () => {
    setAgentKeyChoice(undefined);
    setFailure(undefined);
    onClose();
  };

  async function apply(add: CatalogSelection[], removeNames: string[]) {
    if (!agent || !targetReady) return;
    setSaving(true);
    setFailure(undefined);
    try {
      if (referenced) {
        if (!template.data) throw new Error("The referenced template could not be read.");
        const nextSkills = applySkillSelections(template.data.resource.spec.skills ?? [], add, removeNames);
        await apiClient.agentBuildingBlocks.updateAgentTemplate({
          namespace: agent.namespace,
          name: referenced,
          resource: {
            metadata: template.data.resource.metadata,
            spec: { ...template.data.resource.spec, skills: nextSkills },
          },
        });
        await invalidateTemplates();
        toast.success(`Template ${referenced} saved`);
      } else {
        const inline = agent.resource.spec.template.inline;
        if (!inline) throw new Error("The agent has no template to change.");
        const nextSkills = applySkillSelections(inline.skills ?? [], add, removeNames);
        await apiClient.agentBuildingBlocks.updateAgent({
          namespace: agent.namespace,
          name: agent.name,
          resource: {
            metadata: agent.resource.metadata,
            spec: { ...agent.resource.spec, template: { inline: { ...inline, skills: nextSkills } } },
          },
        });
        await invalidateAgents();
        toast.success(`Agent ${agent.name} saved`);
      }
      close();
    } catch (cause: unknown) {
      setFailure(cause instanceof Error ? cause.message : String(cause));
    } finally {
      setSaving(false);
    }
  }

  const muted = { color: theme.color.textMuted, fontSize: 12 };

  return (
    <Modal
      open={open}
      onCancel={saving ? undefined : close}
      closable={!saving}
      maskClosable={false}
      footer={null}
      width={760}
      title={initialAdd.length > 0 ? "Add skills to agent" : "Manage agent skills"}
      destroyOnHidden
      data-testid="agent-skills-dialog"
    >
      <Space orientation="vertical" size={16} css={{ display: "flex" }}>
        <Space orientation="vertical" size={4} css={{ display: "flex" }}>
          <Text strong>Agent</Text>
          <Select
            showSearch
            css={{ width: "100%" }}
            placeholder="Choose an agent"
            value={agentKeyChoice}
            onChange={(value: string) => {
              setAgentKeyChoice(value);
              setFailure(undefined);
            }}
            options={agents.map((agent) => ({
              value: agentKey(agent),
              label: `${agent.namespace}/${agent.name}`,
            }))}
            optionFilterProp="label"
            aria-label="Choose an agent"
            data-testid="agent-skills-agent-picker"
          />
        </Space>

        {!agentKeyChoice ? (
          <>
            {readFailure ? (
              <Alert
                type="error"
                showIcon
                data-testid="agent-skills-agents-error"
                title="Could not read the agents list"
                description={readFailure instanceof Error ? readFailure.message : String(readFailure)}
              />
            ) : null}
            <Paragraph css={muted}>
              Skills live on the agent's template. Adding or removing them here writes that template; an
              inline template changes only this agent, a referenced one every agent made from it.
            </Paragraph>
          </>
        ) : !agent ? (
          <Spin size="small" />
        ) : (
          <>
            {referenced ? (
              <Alert
                type={sharedWith.length > 1 ? "warning" : "info"}
                showIcon
                data-testid="agent-skills-shared-template"
                title={
                  sharedWith.length > 1
                    ? `Template ${referenced} is shared by ${sharedWith.length} agents`
                    : `Template ${referenced} is referenced by this agent`
                }
                description="Saving changes the template, so every agent made from it gets the same skills."
              />
            ) : (
              <Alert type="info" showIcon title="Inline template" description="Changes affect this agent only." />
            )}

            {!catalog ? (
              <Text css={muted}>
                No skills catalog is configured for this install, so there is nothing to add here.
                Skills the agent already has stay untouched.
              </Text>
            ) : !targetReady ? (
              <Spin size="small" />
            ) : initialAdd.length > 0 ? (
              <div css={{ display: "flex", flexDirection: "column", gap: 12 }}>
                <div css={{ display: "flex", flexDirection: "column", gap: 4 }}>
                  {initialAdd.map((selection) => (
                    <Text key={selection.name} data-testid={`agent-skills-added-${selection.name}`}>
                      {selection.name}
                      {selection.image === catalog.image ? null : (
                        <Text css={muted}> — at a chosen version</Text>
                      )}
                    </Text>
                  ))}
                </div>
                {failure ? <Alert type="error" showIcon title="Save failed" description={failure} /> : null}
                <div css={{ display: "flex", justifyContent: "flex-end" }}>
                  <Button
                    type="primary"
                    loading={saving}
                    data-testid="agent-skills-save"
                    onClick={() => apply(initialAdd, [])}
                  >
                    Save {initialAdd.length === 1 ? "1 skill" : `${initialAdd.length} skills`}
                  </Button>
                </div>
              </div>
            ) : (
              <>
                {failure ? <Alert type="error" showIcon title="Save failed" description={failure} /> : null}
                <SkillsCatalogBrowser
                  catalog={catalog}
                  preChecked={preChecked}
                  disabledNames={unmanaged}
                  applyLabel="Save"
                  onApply={apply}
                />
              </>
            )}
          </>
        )}
      </Space>
    </Modal>
  );
}
