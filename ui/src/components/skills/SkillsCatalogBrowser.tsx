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
 *   Untouched rows are left exactly as held — the apply never re-sends one — so
 *   removing an unrelated skill cannot quietly move a row pinned at an older
 *   digest onto the catalog's current one; `heldImages` marks such rows
 *   "Older version" instead, the same indication the template form uses.
 * - `disabledNames` — the target holds a skill of this name the catalog does not
 *   own (a custom source). Shown "Added" and not checkable: the catalog cannot
 *   manage what it did not provide, so the row must not pretend otherwise.
 * - otherwise — checkable; checking queues an addition.
 */
export function SkillsCatalogBrowser({
  catalog,
  preChecked = [],
  disabledNames = [],
  heldImages = {},
  applyLabel = "Add",
  onApply,
}: {
  catalog: BuiltinSkillsCatalog;
  /** Catalog skill names to check on open; unchecking one removes it. */
  preChecked?: string[];
  /** Skill names the target holds that the catalog does not own; not checkable. */
  disabledNames?: string[];
  /** For a pre-checked name, the digest-pinned image the target actually holds. */
  heldImages?: Record<string, string>;
  /** The apply button's prefix; the selected count follows it. */
  applyLabel?: string;
  /**
   * Called with the additions and removals the apply carries. May return `false`
   * — directly or through the promise — when the apply did not land; the browser
   * then keeps every selection and version choice for a retry instead of
   * resetting to the opening state.
   */
  onApply: (add: CatalogSelection[], removeNames: string[]) => boolean | void | Promise<boolean | void>;
}) {
  const theme = useTheme();
  const [search, setSearch] = useState("");
  const [activeTags, setActiveTags] = useState<string[]>([]);
  // One version choice per checked-or-checkable skill; absent means "current".
  const [versionBySkill, setVersionBySkill] = useState<Record<string, number>>({});
  const [checked, setChecked] = useState<Set<string>>(() => new Set(preChecked));
  // Rows the reader has acted on — checked, unchecked, or re-pinned — since the
  // browser opened. Only these join the apply: a pre-checked row left as it was
  // is already on the target at whatever pin it was saved with, and re-sending
  // it would silently upgrade that pin to the catalog's current digest.
  const [touched, setTouched] = useState<Set<string>>(() => new Set());

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

  /**
   * The applied additions: checked rows the reader actually touched. Unchecking
   * and re-pinning are touches, so everything an apply would change is here —
   * and nothing else is, which is what keeps a removal from carrying silent
   * re-pins of the rows left alone.
   */
  const selections = (): CatalogSelection[] =>
    catalog.skills
      .filter((skill) => checked.has(skill.name) && touched.has(skill.name))
      .map((skill) => {
        const at = versionBySkill[skill.name];
        return selectionForSkill(skill, at === undefined ? undefined : skill.versions[at], catalog.image);
      });
  // A pre-checked skill that the reader unchecked is a removal, not an omission.
  const removals = preChecked.filter((name) => !checked.has(name));

  /*
   * The change set the apply would actually write: rows checked that the target
   * does not hold, held rows re-pinned to a digest they are not held at, and
   * removals. A held row sitting exactly as it was — or put back to it — stages
   * nothing, which is what keeps Save dark on a manage view nobody has changed
   * while still letting unchecking the *last* held skill (nothing left checked)
   * enable it. A held row with no pin on record counts as changed: a write the
   * target may not need beats a Save the reader cannot press.
   */
  const stagedCount =
    selections().filter(
      (selection) =>
        !preChecked.includes(selection.name) || heldImages[selection.name] !== selection.image,
    ).length + removals.length;

  const touch = (name: string) =>
    setTouched((current) => new Set(current).add(name));

  const toggle = (name: string) => {
    touch(name);
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
    // The pin this row would apply as things stand; a held row sitting on some
    // other digest is marked rather than quietly re-pinned by the next save.
    const appliedImage = chosenAt === undefined ? catalog.image : skill.versions[chosenAt]?.image ?? catalog.image;
    const olderPin = heldImages[skill.name] !== undefined && heldImages[skill.name] !== appliedImage;
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
              {olderPin ? (
                <Tag
                  color="warning"
                  css={{ marginInlineEnd: 0 }}
                  data-testid={`skills-catalog-outdated-${skill.name}`}
                  title="Saved from an earlier kagent-tools image than this install ships. It stays as held unless a version is chosen for it here."
                >
                  Older version
                </Tag>
              ) : null}
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
              touch(skill.name);
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
          disabled={stagedCount === 0}
          data-testid="skills-catalog-apply"
          onClick={() => {
            const applied = onApply(selections(), removals);
            // Reset only once the caller has confirmed the apply landed. The
            // manage dialog keeps this modal open when a save fails, and wiping
            // the checkboxes and version choices then would make the reader
            // rebuild the whole selection to retry.
            void Promise.resolve(applied).then((outcome) => {
              if (outcome !== false) {
                setChecked(new Set(preChecked));
                setTouched(new Set());
                setVersionBySkill({});
              }
            });
          }}
        >
          {/* The count is what Save writes — the staged change set — not the
              checkbox count above, which includes held rows left as they are. */}
          {applyLabel}
          {stagedCount > 0 ? ` ${stagedCount === 1 ? "1 skill" : `${stagedCount} skills`}` : ""}
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

