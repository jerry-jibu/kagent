import type { AppExtensionConfig } from "@/appExtensions";
import { SkillsCatalogNavItem } from "./SkillsCatalogNavItem";
import { SkillsCatalogPage } from "./SkillsCatalogPage";
import { SKILLS_CATALOG_PATH } from "./paths";

/**
 * kagent's skills-catalog surface, as a first-party extension.
 *
 * Everything it needs arrives through the extension points — a nav entry, a
 * route, and components that compose core modules (`SkillsCatalogBrowser`, the
 * agent APIs) — so it is also the worked proof that the catalog UI needs no
 * special casing in the application itself. Installing it is its entry in
 * `activeExtensions.ts`; it ships on by default.
 */
export const skillsCatalogExtension: AppExtensionConfig = {
  id: "skills-catalog",
  name: "Skills Catalog",

  // Between Agents (200) and Models (300), where a catalog of skills reads best.
  navItems: [
    { key: "skills-catalog", order: 250, path: SKILLS_CATALOG_PATH, Component: SkillsCatalogNavItem },
  ],

  routes: [{ path: SKILLS_CATALOG_PATH, element: <SkillsCatalogPage /> }],
};
