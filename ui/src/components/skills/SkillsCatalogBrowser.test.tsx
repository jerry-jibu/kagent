import { fireEvent, render, screen } from "@testing-library/react";
import { ThemeProvider } from "@emotion/react";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { themeFor } from "@/theme/theme";
import { mockBuiltinSkills } from "@/mocks/fixtures";
import { SkillsCatalogBrowser } from "./SkillsCatalogBrowser";
import type { CatalogSelection } from "./catalogSelection";

afterEach(() => vi.restoreAllMocks());

function Browser(props: Partial<Parameters<typeof SkillsCatalogBrowser>[0]>) {
  const [last, setLast] = useState<{ add: CatalogSelection[]; remove: string[] }>();
  return (
    <ThemeProvider theme={themeFor("dark")}>
      <SkillsCatalogBrowser
        catalog={mockBuiltinSkills}
        applyLabel="Add"
        onApply={(add, remove) => setLast({ add, remove })}
        {...props}
      />
      {last ? <output data-testid="applied">{JSON.stringify(last)}</output> : null}
    </ThemeProvider>
  );
}

/** Opens an antd Select by its test id and clicks the option whose content reads `option`. */
async function chooseOption(testId: string, option: string) {
  fireEvent.mouseDown(screen.getByTestId(testId));
  const content = await screen.findByText(option, { selector: ".ant-select-item-option-content" });
  fireEvent.click(content.closest(".ant-select-item-option")!);
}

const currentImage = mockBuiltinSkills.image;
const olderVersion = mockBuiltinSkills.skills[3].versions[0];

it("Given a catalog, when the reader searches, assert that only matching skills render", async () => {
  render(<Browser />);
  expect(screen.getAllByTestId(/^skills-catalog-card-/).length).toBe(mockBuiltinSkills.skills.length);
  fireEvent.change(screen.getByTestId("skills-catalog-search"), { target: { value: "troubleshooting" } });
  const cards = screen.getAllByTestId(/^skills-catalog-card-/);
  expect(cards.map((card) => card.dataset.testid)).toEqual(["skills-catalog-card-k8s-troubleshooting"]);
  fireEvent.change(screen.getByTestId("skills-catalog-search"), { target: { value: "nothing-matches" } });
  expect(screen.getByText("No skills match.")).toBeDefined();
});

it("Given a tag filter, when the reader picks tags, assert that only skills carrying every tag render", async () => {
  render(<Browser />);
  await chooseOption("skills-catalog-tag-filter", "kubernetes");
  const names = screen
    .getAllByTestId(/^skills-catalog-card-/)
    .map((card) => card.dataset.testid?.replace("skills-catalog-card-", ""));
  expect(names).toEqual(["helm-release-management", "k8s-troubleshooting"]);
});

it("Given two checked skills, one at a chosen older version, when applying, assert that the selections carry each version's pin", async () => {
  render(<Browser />);
  fireEvent.click(screen.getByTestId("skills-catalog-check-k8s-troubleshooting"));
  fireEvent.click(screen.getByTestId("skills-catalog-check-prometheus-observability"));
  await chooseOption("skills-catalog-version-k8s-troubleshooting", olderVersion.label!);
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const applied = JSON.parse(screen.getByTestId("applied").textContent!) as {
    add: CatalogSelection[];
    remove: string[];
  };
  expect(applied.remove).toEqual([]);
  const k8s = applied.add.find((selection) => selection.name === "k8s-troubleshooting");
  const prometheus = applied.add.find((selection) => selection.name === "prometheus-observability");
  expect(k8s?.image).toBe(olderVersion.image);
  expect(k8s?.path).toBe("opt/kagent-tools-skills/skills/k8s-troubleshooting");
  expect(prometheus?.image).toBe(currentImage);
});

it("Given a name the target already holds, when the browser renders, assert that the row is locked and marked Added", () => {
  render(<Browser disabledNames={["k8s-troubleshooting"]} />);
  const checkbox = screen.getByTestId("skills-catalog-check-k8s-troubleshooting");
  expect(checkbox).toHaveProperty("disabled", true);
  expect(screen.getByTestId("skills-catalog-card-k8s-troubleshooting").textContent).toContain("Added");
});

it("Given a pre-checked skill, when the reader unchecks it and applies, assert that the apply names it as a removal", () => {
  render(<Browser preChecked={["k8s-troubleshooting", "prometheus-observability"]} />);
  fireEvent.click(screen.getByTestId("skills-catalog-check-prometheus-observability"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const applied = JSON.parse(screen.getByTestId("applied").textContent!) as {
    add: CatalogSelection[];
    remove: string[];
  };
  expect(applied.remove).toEqual(["prometheus-observability"]);
  expect(applied.add.map((selection) => selection.name)).toEqual(["k8s-troubleshooting"]);
});

it("Given nothing checked, when applying, assert that the apply button is disabled", () => {
  render(<Browser />);
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", true);
});
