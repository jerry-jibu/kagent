import { useMemo, useState } from "react";
import { Button, Card, Checkbox, Input, Select, Tag, Typography } from "antd";
import { useTheme } from "@emotion/react";
import { Search } from "lucide-react";
import type { BuiltinSkillsCatalog } from "@/api/runtimeConfig";
import { selectionForSkill, type CatalogSelection } from "./catalogSelection";

const { Text } = Typography;

/**
 * The catalog's skills as searchable, filterable, selectable cards, with a
 * version select per skill.
 *
 * Shared by the template form's picker modal and the skills-catalog page, so
 * "what a catalog skill looks like" has one rendering. Filtering and version
 * choices are the browser's own state; the caller learns only the applied
 * result — additions and removals — through `onApply`.
 *
 * Three relationship states a row can be in, all visible at once:
 * - `preChecked` — the target already holds this catalog skill; checked on open
 *   and version-editable, because unchecking is how a manage dialog removes.
 * - `disabledNames` — the target holds a skill of this name the catalog does not
 *   own (a custom source). Shown "Added" and not checkable: the catalog cannot
 *   manage what it did not provide, so the row must not pretend otherwise.
 * - otherwise — checkable; checking queues an addition.
 */
export function SkillsCatalogBrowser({
  catalog,
  preChecked = [],
  disabledNames = [],
  applyLabel = "Add",
  onApply,
}: {
  catalog: BuiltinSkillsCatalog;
  /** Catalog skill names to check on open; unchecking one removes it. */
  preChecked?: string[];
  /** Skill names the target holds that the catalog does not own; not checkable. */
  disabledNames?: string[];
  /** The apply button's prefix; the selected count follows it. */
  applyLabel?: string;
  onApply: (add: CatalogSelection[], removeNames: string[]) => void;
}) {
  const theme = useTheme();
  const [search, setSearch] = useState("");
  const [activeTags, setActiveTags] = useState<string[]>([]);
  // One version choice per checked-or-checkable skill; absent means "current".
  const [versionBySkill, setVersionBySkill] = useState<Record<string, number>>({});
  const [checked, setChecked] = useState<Set<string>>(() => new Set(preChecked));

  const allTags = useMemo(
    () => [...new Set(catalog.skills.flatMap((skill) => skill.tags))].sort(),
    [catalog],
  );
  const disabled = useMemo(() => new Set(disabledNames), [disabledNames]);

  const visible = useMemo(() => {
    const query = search.trim().toLowerCase();
    return catalog.skills.filter((skill) => {
      const matchesQuery =
        query === "" ||
        skill.name.toLowerCase().includes(query) ||
        skill.description.toLowerCase().includes(query);
      const matchesTags = activeTags.every((tag) => skill.tags.includes(tag));
      return matchesQuery && matchesTags;
    });
  }, [catalog, search, activeTags]);

  /** The applied state: checked catalog skills become selections at their chosen version. */
  const selections = (): CatalogSelection[] =>
    catalog.skills
      .filter((skill) => checked.has(skill.name))
      .map((skill) => {
        const at = versionBySkill[skill.name];
        return selectionForSkill(skill, at === undefined ? undefined : skill.versions[at], catalog.image);
      });
  // A pre-checked skill that the reader unchecked is a removal, not an omission.
  const removals = preChecked.filter((name) => !checked.has(name));

  const toggle = (name: string) => {
    setChecked((current) => {
      const next = new Set(current);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  const muted = { color: theme.color.textMuted, fontSize: 12 };

  const card = (skill: (typeof catalog.skills)[number]) => {
    const versions = [{ label: "Latest", image: catalog.image, path: skill.path }, ...skill.versions];
    const chosenAt = versionBySkill[skill.name];
    const isDisabled = disabled.has(skill.name);
    return (
      <Card
        key={skill.name}
        size="small"
        css={{ borderColor: theme.color.border, display: "flex", flexDirection: "column", gap: 6 }}
        data-testid={`skills-catalog-card-${skill.name}`}
      >
        <div css={{ display: "flex", alignItems: "flex-start", gap: 8 }}>
          <Checkbox
            checked={checked.has(skill.name)}
            disabled={isDisabled}
            onChange={() => toggle(skill.name)}
            aria-label={`Select skill ${skill.name}`}
            data-testid={`skills-catalog-check-${skill.name}`}
            css={{ marginTop: 2 }}
          />
          <div css={{ minWidth: 0, flex: 1 }}>
            <div css={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
              <Text strong>{skill.name}</Text>
              {isDisabled ? <Tag css={{ marginInlineEnd: 0 }}>Added</Tag> : null}
              {skill.tags.map((tag) => (
                <Tag key={tag} css={{ marginInlineEnd: 0 }} data-testid={`skills-catalog-tag-${tag}`}>
                  {tag}
                </Tag>
              ))}
            </div>
            <Text css={{ ...muted, display: "block", whiteSpace: "normal" }}>{skill.description}</Text>
          </div>
        </div>
        {skill.versions.length > 0 && !isDisabled ? (
          <Select
            size="small"
            variant="filled"
            css={{ width: "100%" }}
            value={chosenAt === undefined ? "current" : String(chosenAt)}
            onChange={(value: string) => {
              setVersionBySkill((current) => {
                const next = { ...current };
                if (value === "current") delete next[skill.name];
                else next[skill.name] = Number(value);
                return next;
              });
            }}
            options={versions.map((version, at) => ({
              value: at === 0 ? "current" : String(at - 1),
              label: version.label ?? shortDigest(version.image),
            }))}
            aria-label={`Version for skill ${skill.name}`}
            data-testid={`skills-catalog-version-${skill.name}`}
          />
        ) : null}
      </Card>
    );
  };

  return (
    <div css={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div css={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <Input
          allowClear
          css={{ flex: "1 1 220px", minWidth: 180 }}
          prefix={<Search size={14} aria-hidden />}
          placeholder="Search skills"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          aria-label="Search skills"
          data-testid="skills-catalog-search"
        />
        {allTags.length > 0 ? (
          <Select
            mode="multiple"
            allowClear
            placeholder="Filter by tag"
            css={{ flex: "1 1 200px", minWidth: 160 }}
            value={activeTags}
            onChange={setActiveTags}
            options={allTags.map((tag) => ({ value: tag, label: tag }))}
            aria-label="Filter skills by tag"
            data-testid="skills-catalog-tag-filter"
          />
        ) : null}
      </div>

      {visible.length === 0 ? (
        <Text css={muted}>No skills match.</Text>
      ) : (
        <div css={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(280px, 1fr))", gap: 10 }}>
          {visible.map(card)}
        </div>
      )}

      <div css={{ display: "flex", alignItems: "center", gap: 12, justifyContent: "flex-end" }}>
        <Text css={muted} data-testid="skills-catalog-count">
          {checked.size} selected
        </Text>
        <Button
          type="primary"
          disabled={checked.size === 0}
          data-testid="skills-catalog-apply"
          onClick={() => {
            onApply(selections(), removals);
            setChecked(new Set(preChecked));
            setVersionBySkill({});
          }}
        >
          {applyLabel} {checked.size === 1 ? "1 skill" : `${checked.size} skills`}
        </Button>
      </div>
    </div>
  );
}

/** A digest pinned version shows its leading hex when the catalog gave no label. */
function shortDigest(image: string): string {
  const digest = image.split("@")[1] ?? "";
  return digest.slice(0, "sha256:".length + 8) + "…";
}

