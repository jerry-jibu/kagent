import { useMemo, useState } from "react";
import { Button, Typography } from "antd";
import { useTheme } from "@emotion/react";
import { PageFrame } from "@/components/Structure/PageFrame";
import { runtimeConfig } from "@/api/runtimeConfig";
import { SkillsCatalogBrowser } from "@/components/skills/SkillsCatalogBrowser";
import type { CatalogSelection } from "@/components/skills/catalogSelection";
import { AgentSkillsDialog } from "./AgentSkillsDialog";

const { Paragraph } = Typography;

/**
 * The whole page this extension contributes: the catalog to browse and pick
 * from, and the hand-off to an agent.
 *
 * Picking and targeting are deliberately separate. The browser below owns
 * search, tags, versions and the selection; "Add to agent…" carries the applied
 * selections into the dialog, which is where the only state that can decide
 * where skills are written lives — the agent, its template shape, and who else
 * shares that template.
 */
export function SkillsCatalogPage() {
  const theme = useTheme();
  const catalog = useMemo(() => runtimeConfig().builtinSkills, []);
  const [dialogOpen, setDialogOpen] = useState(false);
  const [pendingAdd, setPendingAdd] = useState<CatalogSelection[]>([]);

  return (
    <PageFrame
      title="Skills catalog"
      description="Browse the skills this install ships, and add them to an agent. A skill is a set of instructions the agent loads when a task matches it."
      actions={
        <Button
          data-testid="skills-catalog-manage"
          onClick={() => setDialogOpen(true)}
        >
          Manage skills on an agent…
        </Button>
      }
    >
      {catalog ? (
        <>
          <SkillsCatalogBrowser
            catalog={catalog}
            applyLabel="Add to agent…"
            onApply={(add) => {
              setPendingAdd(add);
              setDialogOpen(true);
            }}
          />
          <AgentSkillsDialog
            open={dialogOpen}
            initialAdd={pendingAdd}
            onClose={() => {
              setDialogOpen(false);
              setPendingAdd([]);
            }}
          />
        </>
      ) : (
        <Paragraph css={{ color: theme.color.textMuted }} data-testid="skills-catalog-empty">
          No skills catalog is configured for this install. The chart offers one from the kagent-tools
          image when built-ins are enabled; without it, skills are still managed on each template with
          kubectl.
        </Paragraph>
      )}
    </PageFrame>
  );
}
