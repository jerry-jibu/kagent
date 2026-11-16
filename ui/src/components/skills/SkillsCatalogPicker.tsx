import { Modal } from "antd";
import type { BuiltinSkillsCatalog } from "@/api/runtimeConfig";
import { SkillsCatalogBrowser } from "./SkillsCatalogBrowser";
import type { CatalogSelection } from "./catalogSelection";

/**
 * The catalog browser in a modal, as the template form's "Browse catalog…"
 * opens it. Additions flow to the caller; nothing here removes, so every name
 * the target already holds is shown "Added" and locked.
 */
export function SkillsCatalogPicker({
  open,
  catalog,
  disabledNames = [],
  title = "Skills catalog",
  onClose,
  onApply,
}: {
  open: boolean;
  catalog: BuiltinSkillsCatalog | undefined;
  /** Skill names the target already holds; same-named rows are locked as "Added". */
  disabledNames?: string[];
  title?: string;
  onClose: () => void;
  onApply: (add: CatalogSelection[]) => void;
}) {
  if (!catalog) return null;
  return (
    <Modal
      open={open}
      onCancel={onClose}
      footer={null}
      title={title}
      width={760}
      destroyOnHidden
      data-testid="skills-catalog-picker"
    >
      <SkillsCatalogBrowser
        catalog={catalog}
        disabledNames={disabledNames}
        applyLabel="Add"
        onApply={(add) => {
          onApply(add);
          onClose();
        }}
      />
    </Modal>
  );
}
