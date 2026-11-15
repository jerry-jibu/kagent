import { fireEvent, render, screen } from "@testing-library/react";
import { ThemeProvider } from "@emotion/react";
import { SWRConfig } from "swr";
import { afterEach, expect, it, vi } from "vitest";
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

it("Given a template holding catalog and custom skills, when a pre-checked skill is unchecked, assert that only it is removed", async () => {
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

  fireEvent.click(screen.getByTestId("skills-catalog-check-helm-release-management"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const template = await apiClient.agentBuildingBlocks.agentTemplate("kagent", "k8s-agent-7f3a91c");
  const names = template.resource.spec.skills?.map((skill) => skill.name) ?? [];
  expect(names).toContain("k8s-troubleshooting");
  expect(names).toContain("incident-review");
  expect(names).not.toContain("helm-release-management");
});

it("Given no catalog configured, when an agent is chosen, assert that the dialog says there is nothing to add", async () => {
  startBackend("");
  renderDialog([]);
  await chooseOption("agent-skills-agent-picker", "kagent/release-notes");
  expect(
    await screen.findByText(/No skills catalog is configured/),
  ).toBeDefined();
});
