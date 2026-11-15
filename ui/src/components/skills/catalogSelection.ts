/**
 * Catalog selections and how they land in `spec.skills`.
 *
 * Pure operations, no I/O and no React: the catalog picker modal, the catalog
 * page and the add/remove-to-agent dialog all produce the same `CatalogSelection`
 * shape and merge it through `applySkillSelections`, so "what the user picked"
 * and "what the resource ends up holding" have exactly one definition.
 */

import type { AgentTemplateSkill } from "@/api/domain/agentTemplates";
import type { BuiltinSkill, BuiltinSkillVersion, BuiltinSkillsCatalog } from "@/api/runtimeConfig";

/** One skill, at one chosen version, as a picker hands it on. */
export interface CatalogSelection {
  name: string;
  description: string;
  /** The digest-pinned image this selection's content comes from. */
  image: string;
  /** The skill's directory inside `image`. */
  path: string;
  tags: string[];
}

/**
 * The selection for a skill at the catalog's current pin, or at `version` when
 * the picker's version select chose one. A version may read the skill from its
 * own directory inside its image; absent, the skill's catalog path applies.
 */
export function selectionForSkill(
  skill: BuiltinSkill,
  version: BuiltinSkillVersion | undefined,
  catalogImage: string,
): CatalogSelection {
  return {
    name: skill.name,
    description: skill.description,
    image: version ? version.image : catalogImage,
    path: version?.path ?? skill.path,
    tags: skill.tags,
  };
}

/** The repository part of a digest-pinned reference, before the `@`. */
function imageRepository(image: string): string {
  return image.split("@")[0];
}

/**
 * Whether a spec's skill already holds this catalog skill — name, repository and
 * path all matching, any digest. The same-shape test `matchBuiltinSkill` applies
 * to form rows, over the spec's source instead.
 */
export function matchesCatalogSkill(
  skill: AgentTemplateSkill,
  selection: CatalogSelection,
): boolean {
  const source = skill.source;
  if (!source.oci) return false;
  if (imageRepository(source.oci) !== imageRepository(selection.image)) return false;
  if ((source.path ?? "") !== selection.path) return false;
  return skill.name === selection.name;
}

/**
 * The catalog skill an existing spec skill is, so a catalog page can pre-check
 * rows and manage dialogs can show "on this agent". Undefined for skills the
 * catalog does not know, which must survive every merge untouched.
 */
export function catalogSkillFor(
  skill: AgentTemplateSkill,
  catalog: BuiltinSkillsCatalog | undefined,
): BuiltinSkill | undefined {
  if (!catalog || !skill.source.oci) return undefined;
  if (imageRepository(skill.source.oci) !== imageRepository(catalog.image)) return undefined;
  return catalog.skills.find(
    (entry) => entry.path === (skill.source.path ?? "") && entry.name === skill.name,
  );
}

/**
 * Merge additions and removals into an existing skill list.
 *
 * - Removals drop by exact name. Anything not named here survives untouched,
 *   including skills the catalog has never heard of and custom sources.
 * - Additions append. One exception: re-adding a name whose existing entry is
 *   the *same catalog skill* (a version change in a manage dialog) replaces that
 *   entry, because the reader changed the pin on purpose. A same-named skill the
 *   catalog does not own is left alone — the picker never offers it checked.
 * - A name in both arguments ends up present: removals are applied first, then
 *   additions. The manage dialog never produces that overlap — a checked row is
 *   an addition, an unchecked one a removal — but the merge stays total.
 */
export function applySkillSelections(
  existing: AgentTemplateSkill[],
  add: CatalogSelection[],
  removeNames: string[] = [],
): AgentTemplateSkill[] {
  const removed = new Set(removeNames);
  const result = existing.filter((skill) => !removed.has(skill.name));
  for (const selection of add) {
    const source: AgentTemplateSkill["source"] = { oci: selection.image };
    if (selection.path) source.path = selection.path;
    const at = result.findIndex((skill) => skill.name === selection.name);
    const replaced =
      at !== -1 && matchesCatalogSkill(result[at], selection)
        ? { name: selection.name, source }
        : undefined;
    if (at === -1) result.push({ name: selection.name, source });
    else if (replaced) result[at] = replaced;
  }
  return result;
}

/** The names `add` would bring that `existing` does not already hold. */
export function newSelectionNames(
  existing: AgentTemplateSkill[],
  add: CatalogSelection[],
): string[] {
  const held = new Set(existing.map((skill) => skill.name));
  return add.filter((selection) => !held.has(selection.name)).map((selection) => selection.name);
}
