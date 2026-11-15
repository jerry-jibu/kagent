import { Menu } from "antd";
import { Link, useNavigate } from "react-router-dom";
import { Blocks } from "lucide-react";
import { useThemeMode } from "@/appExtensions";
import type { ExtensionNavItemProps } from "@/appExtensions";
import { SKILLS_CATALOG_PATH } from "./paths";

/**
 * The whole nav entry, drawn the same way `ExampleNavItem` draws its own: the
 * same antd `Menu` the shell uses for its core entries, so the contributed row
 * is indistinguishable from a core one in both palettes and both widths.
 */
export function SkillsCatalogNavItem({ isActive, collapsed }: ExtensionNavItemProps) {
  const { mode } = useThemeMode();
  const navigate = useNavigate();

  return (
    <Menu
      theme={mode === "light" ? "light" : "dark"}
      mode="inline"
      inlineCollapsed={collapsed}
      selectedKeys={isActive ? ["skills-catalog"] : []}
      css={{ borderInlineEnd: "none" }}
      onClick={({ domEvent }) => {
        if ((domEvent.target as HTMLElement).closest("a")) return;
        navigate(SKILLS_CATALOG_PATH);
      }}
      items={[
        {
          key: "skills-catalog",
          icon: <Blocks size={16} />,
          label: (
            <Link to={SKILLS_CATALOG_PATH} css={{ color: "inherit" }}>
              Skills catalog
            </Link>
          ),
          "data-testid": "nav-skills-catalog",
        },
      ]}
    />
  );
}
