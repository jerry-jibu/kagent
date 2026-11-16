import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

it("Given two tags selected, when a skill carries only one of them, assert that it is filtered out", async () => {
  render(<Browser />);
  // Both picks come from the one open dropdown: a multiple select stays open
  // after a pick, so the second option is clicked without reopening it.
  fireEvent.mouseDown(screen.getByTestId("skills-catalog-tag-filter"));
  const kubernetes = await screen.findByText("kubernetes", { selector: ".ant-select-item-option-content" });
  fireEvent.click(kubernetes.closest(".ant-select-item-option")!);
  const delivery = await screen.findByText("delivery", { selector: ".ant-select-item-option-content" });
  fireEvent.click(delivery.closest(".ant-select-item-option")!);
  const names = screen
    .getAllByTestId(/^skills-catalog-card-/)
    .map((card) => card.dataset.testid?.replace("skills-catalog-card-", ""));
  // helm-release-management carries both tags; k8s-troubleshooting carries only
  // "kubernetes", and the AND across the two tags must exclude it.
  expect(names).toEqual(["helm-release-management"]);
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

it("Given a pre-checked skill, when the reader unchecks it and applies, assert that the apply names it as a removal and leaves the untouched held row out", () => {
  render(<Browser preChecked={["k8s-troubleshooting", "prometheus-observability"]} />);
  fireEvent.click(screen.getByTestId("skills-catalog-check-prometheus-observability"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const applied = JSON.parse(screen.getByTestId("applied").textContent!) as {
    add: CatalogSelection[];
    remove: string[];
  };
  expect(applied.remove).toEqual(["prometheus-observability"]);
  // k8s-troubleshooting was pre-checked and left alone, so it is not re-sent:
  // re-sending it would pin whatever digest the catalog currently ships, which
  // is a silent upgrade of a row the reader never touched.
  expect(applied.add).toEqual([]);
});

it("Given nothing checked, when applying, assert that the apply button is disabled", () => {
  render(<Browser />);
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", true);
});

it("Given a pre-checked skill held at an older digest, when the browser renders, assert that the row is marked Older version", () => {
  const olderImage = "ghcr.io/kagent-dev/kagent/tools@sha256:" + "1".repeat(64);
  render(
    <Browser preChecked={["prometheus-observability"]} heldImages={{ "prometheus-observability": olderImage }} />,
  );
  expect(
    screen.getByTestId("skills-catalog-outdated-prometheus-observability").textContent,
  ).toContain("Older version");
  // A row held at the current pin carries no mark.
  expect(screen.queryByTestId("skills-catalog-outdated-k8s-troubleshooting")).toBeNull();
});

it("Given a held row pinned older than the catalog, when the reader removes an unrelated skill, assert that the older row is not re-pinned", () => {
  const olderImage = "ghcr.io/kagent-dev/kagent/tools@sha256:" + "1".repeat(64);
  render(
    <Browser
      preChecked={["k8s-troubleshooting", "prometheus-observability"]}
      heldImages={{ "prometheus-observability": olderImage }}
    />,
  );
  fireEvent.click(screen.getByTestId("skills-catalog-check-k8s-troubleshooting"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const applied = JSON.parse(screen.getByTestId("applied").textContent!) as {
    add: CatalogSelection[];
    remove: string[];
  };
  expect(applied.remove).toEqual(["k8s-troubleshooting"]);
  // Nothing touched prometheus-observability, so nothing re-pins it: the merge
  // must not upgrade its older digest as a side effect of another removal.
  expect(applied.add).toEqual([]);
});

it("Given a held row checked and left exactly as held, when nothing is staged, assert that the apply button is disabled", () => {
  render(<Browser preChecked={["k8s-troubleshooting"]} />);
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", true);
});

it("Given a held row pinned older than the catalog, when the reader re-pins it, assert that only a pin the target does not hold enables Save", async () => {
  render(
    <Browser
      preChecked={["k8s-troubleshooting"]}
      heldImages={{ "k8s-troubleshooting": olderVersion.image }}
    />,
  );
  expect(screen.getByTestId("skills-catalog-outdated-k8s-troubleshooting").textContent).toContain(
    "Older version",
  );
  // Choosing the digest the target already holds stages nothing: the row would
  // be written back exactly as held, and the mark resolves because the row now
  // agrees with what the target has.
  await chooseOption("skills-catalog-version-k8s-troubleshooting", olderVersion.label!);
  expect(screen.queryByTestId("skills-catalog-outdated-k8s-troubleshooting")).toBeNull();
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", true);

  // Choosing the catalog's current pin is a real re-pin, and Save comes alive.
  await chooseOption("skills-catalog-version-k8s-troubleshooting", "Latest");
  expect(screen.getByTestId("skills-catalog-apply")).toHaveProperty("disabled", false);
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  const applied = JSON.parse(screen.getByTestId("applied").textContent!) as {
    add: CatalogSelection[];
    remove: string[];
  };
  expect(applied.add.map((selection) => selection.name)).toEqual(["k8s-troubleshooting"]);
  expect(applied.add[0]?.image).toBe(currentImage);
});

it("Given an apply the caller reports as failed, when it settles, assert that the selections and version choices survive for a retry", async () => {
  render(
    <Browser preChecked={["prometheus-observability"]} onApply={() => Promise.resolve(false)} />,
  );
  fireEvent.click(screen.getByTestId("skills-catalog-check-k8s-troubleshooting"));
  await chooseOption("skills-catalog-version-k8s-troubleshooting", olderVersion.label!);
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  await waitFor(() => {
    // The failed apply must not have reset anything: the dialog stays open on a
    // failed save, and the reader would otherwise rebuild the whole selection.
    expect(screen.getByTestId("skills-catalog-check-k8s-troubleshooting")).toHaveProperty("checked", true);
    expect(screen.getByTestId("skills-catalog-count").textContent).toContain("2 selected");
    expect(screen.getByTestId("skills-catalog-version-k8s-troubleshooting").textContent).toContain(
      olderVersion.label!,
    );
  });
});

it("Given an apply the caller confirms, when it settles, assert that the selection resets to the opening state", async () => {
  render(
    <Browser preChecked={["prometheus-observability"]} onApply={() => Promise.resolve(true)} />,
  );
  fireEvent.click(screen.getByTestId("skills-catalog-check-k8s-troubleshooting"));
  fireEvent.click(screen.getByTestId("skills-catalog-apply"));

  await waitFor(() => {
    expect(screen.getByTestId("skills-catalog-check-k8s-troubleshooting")).toHaveProperty("checked", false);
    // Back to exactly the pre-checked state the browser opened with.
    expect(screen.getByTestId("skills-catalog-count").textContent).toContain("1 selected");
    expect(screen.getByTestId("skills-catalog-check-prometheus-observability")).toHaveProperty("checked", true);
  });
});
