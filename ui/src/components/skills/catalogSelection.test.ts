import { describe, expect, it } from "vitest";
import type { AgentTemplateSkill } from "@/api/domain/agentTemplates";
import type { BuiltinSkill, BuiltinSkillsCatalog } from "@/api/runtimeConfig";
import {
  applySkillSelections,
  catalogSkillFor,
  matchesCatalogSkill,
  newSelectionNames,
  selectionForSkill,
  skippedAdditions,
  type CatalogSelection,
} from "./catalogSelection";

const CURRENT = "ghcr.io/kagent-dev/kagent/tools@sha256:" + "a".repeat(64);
const OLDER = "ghcr.io/kagent-dev/kagent/tools@sha256:" + "b".repeat(64);

const skill = (overrides: Partial<BuiltinSkill> = {}): BuiltinSkill => ({
  name: "k8s-troubleshooting",
  description: "Diagnose unhealthy workloads.",
  path: "opt/kagent-tools-skills/skills/k8s-troubleshooting",
  tags: [],
  versions: [],
  ...overrides,
});

const catalog = (skills: BuiltinSkill[] = [skill()]): BuiltinSkillsCatalog => ({ image: CURRENT, skills });

const specSkill = (name: string, image: string, path?: string): AgentTemplateSkill => ({
  name,
  source: { oci: image, ...(path ? { path } : {}) },
});

describe("selectionForSkill", () => {
  it("selects the current pin when no version is chosen", () => {
    expect(selectionForSkill(skill({ tags: ["kubernetes"] }), undefined, CURRENT)).toEqual({
      name: "k8s-troubleshooting",
      description: "Diagnose unhealthy workloads.",
      image: CURRENT,
      path: "opt/kagent-tools-skills/skills/k8s-troubleshooting",
      tags: ["kubernetes"],
    });
  });

  it("selects a version's image, and its own path when the version carries one", () => {
    const moved = selectionForSkill(
      skill({ versions: [{ image: OLDER, path: "skills/elsewhere" }] }),
      skill({ versions: [{ image: OLDER, path: "skills/elsewhere" }] }).versions[0],
      CURRENT,
    );
    expect(moved.image).toBe(OLDER);
    expect(moved.path).toBe("skills/elsewhere");

    const labeled = selectionForSkill(skill({ versions: [{ image: OLDER }] }), skill({ versions: [{ image: OLDER }] }).versions[0], CURRENT);
    expect(labeled.path).toBe("opt/kagent-tools-skills/skills/k8s-troubleshooting");
  });
});

describe("applySkillSelections", () => {
  it("adds a selection as a digest-pinned source with the version's path", () => {
    const add: CatalogSelection[] = [
      { name: "helm-release-management", description: "", image: OLDER, path: "opt/kagent-tools-skills/skills/helm-release-management", tags: [] },
    ];
    expect(applySkillSelections([], add)).toEqual([
      { name: "helm-release-management", source: { oci: OLDER, path: "opt/kagent-tools-skills/skills/helm-release-management" } },
    ]);
  });

  it("removes by name and leaves unknown skills untouched", () => {
    const existing = [
      specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting"),
      { name: "team-runbooks", source: { git: { url: "https://git.test/r.git", commit: "c".repeat(40) } } },
    ];
    const result = applySkillSelections(existing, [], ["k8s-troubleshooting"]);
    expect(result).toEqual([{ name: "team-runbooks", source: { git: { url: "https://git.test/r.git", commit: "c".repeat(40) } } }]);
  });

  it("re-pins the same catalog skill when a manage dialog changes its version", () => {
    const existing = [specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting")];
    const add: CatalogSelection[] = [
      { name: "k8s-troubleshooting", description: "", image: OLDER, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting", tags: [] },
    ];
    expect(applySkillSelections(existing, add)).toEqual([
      { name: "k8s-troubleshooting", source: { oci: OLDER, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting" } },
    ]);
  });

  it("never overwrites a same-named skill the catalog does not own", () => {
    const custom = { name: "k8s-troubleshooting", source: { git: { url: "https://git.test/r.git", commit: "c".repeat(40) } } };
    const add: CatalogSelection[] = [
      { name: "k8s-troubleshooting", description: "", image: CURRENT, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting", tags: [] },
    ];
    expect(applySkillSelections([custom], add)).toEqual([custom]);
  });

  it("re-pins a kept skill, adds a newly checked one, and drops an unchecked one", () => {
    const held = [
      specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting"),
      specSkill("istio-traffic-debugging", CURRENT, "opt/kagent-tools-skills/skills/istio-traffic-debugging"),
    ];
    const add: CatalogSelection[] = [
      { name: "k8s-troubleshooting", description: "", image: OLDER, path: "opt/kagent-tools-skills/skills/k8s-troubleshooting", tags: [] },
      { name: "helm-release-management", description: "", image: CURRENT, path: "opt/kagent-tools-skills/skills/helm-release-management", tags: [] },
    ];
    const result = applySkillSelections(held, add, ["istio-traffic-debugging"]);
    expect(result.map((skill) => skill.name)).toEqual(["k8s-troubleshooting", "helm-release-management"]);
    expect(result[0].source.oci).toBe(OLDER);
  });
});

describe("catalogSkillFor", () => {
  it("matches by repository, path and name, whatever the digest", () => {
    const held = specSkill("k8s-troubleshooting", OLDER, "opt/kagent-tools-skills/skills/k8s-troubleshooting");
    expect(catalogSkillFor(held, catalog())).toEqual(skill());
  });

  it("does not match a different repository, path, or a non-OCI source", () => {
    const c = catalog();
    expect(catalogSkillFor(specSkill("k8s-troubleshooting", "ghcr.io/other/tools" + "@sha256:" + "a".repeat(64), "opt/kagent-tools-skills/skills/k8s-troubleshooting"), c)).toBeUndefined();
    expect(catalogSkillFor(specSkill("k8s-troubleshooting", CURRENT, "opt/elsewhere"), c)).toBeUndefined();
    expect(catalogSkillFor({ name: "team-runbooks", source: { git: { url: "https://git.test/r.git", commit: "c".repeat(40) } } }, c)).toBeUndefined();
    expect(catalogSkillFor(specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting"), undefined)).toBeUndefined();
  });
});

describe("matchesCatalogSkill and newSelectionNames", () => {
  const selection: CatalogSelection = {
    name: "k8s-troubleshooting",
    description: "",
    image: OLDER,
    path: "opt/kagent-tools-skills/skills/k8s-troubleshooting",
    tags: [],
  };

  it("recognizes the same catalog skill across digests and names the genuinely new ones", () => {
    expect(matchesCatalogSkill(specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting"), selection)).toBe(true);
    expect(matchesCatalogSkill(specSkill("k8s-troubleshooting", "ghcr.io/other/tools@sha256:" + "a".repeat(64), "opt/kagent-tools-skills/skills/k8s-troubleshooting"), selection)).toBe(false);
    expect(newSelectionNames([specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting")], [selection])).toEqual([]);
    expect(newSelectionNames([], [selection, { ...selection, name: "helm-release-management", path: "opt/kagent-tools-skills/skills/helm-release-management" }])).toEqual([
      "k8s-troubleshooting",
      "helm-release-management",
    ]);
  });
});

describe("skippedAdditions", () => {
  const selection = (name: string, image: string): CatalogSelection => ({
    name,
    description: "",
    image,
    path: `opt/kagent-tools-skills/skills/${name}`,
    tags: [],
  });

  it("names a pick the target holds under the same name from a source the catalog does not own", () => {
    const custom = { name: "k8s-troubleshooting", source: { git: { url: "https://git.test/r.git", commit: "c".repeat(40) } } };
    expect(skippedAdditions([custom], [selection("k8s-troubleshooting", CURRENT)])).toEqual(["k8s-troubleshooting"]);
  });

  it("does not name a held skill the catalog owns — re-picking it is a re-pin, not a skip", () => {
    const held = [specSkill("k8s-troubleshooting", CURRENT, "opt/kagent-tools-skills/skills/k8s-troubleshooting")];
    expect(skippedAdditions(held, [selection("k8s-troubleshooting", OLDER)])).toEqual([]);
  });

  it("does not name a genuinely new skill", () => {
    expect(skippedAdditions([], [selection("helm-release-management", CURRENT)])).toEqual([]);
  });
});
