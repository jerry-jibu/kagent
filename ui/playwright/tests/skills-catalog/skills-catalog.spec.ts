import { test, expect } from "../../fixtures/test";
import { loadPage } from "../../helpers/app";
import { LIFECYCLE_TIMEOUT, pressOnce, selectOption, optionNamed } from "../../helpers/resource";

/*
 * A lifecycle is longer than a journey, so it gets its own budget — see
 * `LIFECYCLE_TIMEOUT`. Set per file rather than across the suite, so the tight default
 * keeps doing its job everywhere else.
 */
test.describe.configure({ timeout: LIFECYCLE_TIMEOUT });

test("skills catalog: browse, add to an agent, and manage what it holds", async ({
  page,
}) => {
  await test.step("1. the catalog lists the built-in skills, searchable and tag-filtered", async () => {
    await loadPage(page, "/skills-catalog");
    await expect(page.getByTestId("skills-catalog-card-k8s-troubleshooting")).toBeVisible({
      timeout: 30_000,
    });

    // Search narrows on the name and the description.
    await page.getByTestId("skills-catalog-search").fill("service mesh");
    await expect(page.getByTestId(/^skills-catalog-card-/)).toHaveCount(1);
    await expect(page.getByTestId("skills-catalog-card-istio-traffic-debugging")).toBeVisible();
    await page.getByTestId("skills-catalog-search").fill("");

    // Tags narrow to the skills carrying every selected tag.
    await page.getByTestId("skills-catalog-tag-filter").click();
    await pressOnce(optionNamed(page, "kubernetes"));
    await page.keyboard.press("Escape");
    await expect(page.getByTestId(/^skills-catalog-card-/)).toHaveCount(2);
    await page.getByTestId("skills-catalog-tag-filter").click();
    await pressOnce(optionNamed(page, "kubernetes"));
    await page.keyboard.press("Escape");
    await expect(page.getByTestId(/^skills-catalog-card-/)).toHaveCount(6);
  });

  await test.step("2. two picked skills land on an inline-template agent", async () => {
    await page.getByTestId("skills-catalog-check-helm-release-management").check();
    await page.getByTestId("skills-catalog-check-prometheus-observability").check();
    await page.getByTestId("skills-catalog-apply").click();

    // The dialog is where the write's shape is decided, so the agent choice
    // comes after the picks. antd hangs the modal's test id on a zero-size
    // wrapper, so visibility is asserted on the panel's first control.
    await expect(page.getByTestId("agent-skills-agent-picker")).toBeVisible();
    await selectOption(page, "agent-skills-agent-picker", "kagent/release-notes");
    await expect(page.getByText("Inline template")).toBeVisible();
    await page.getByTestId("agent-skills-save").click();
    await expect(page.getByText("Agent release-notes saved")).toBeVisible();
    await expect(page.getByTestId("agent-skills-dialog")).toHaveCount(0);

    // Read-back through the agent's own edit form, not the toast: the spec the
    // backend stored is what matters. The path goes through the UI because the
    // mock backend lives in the page's JS context — a page reload would reset
    // the fixtures and forget the save this step just made.
    await page.getByTestId("nav-agents").click();
    const row = page.getByRole("row").filter({ hasText: "release-notes" });
    await row.getByTestId("edit-release-notes").click();
    await expect(page).toHaveURL(/\/agents\/kagent\/release-notes\/edit$/);
    // Catalog skills read back as builtin cards — recognized, tagged kagent —
    // not as editable rows with a name input.
    await expect(page.getByTestId("template-form-skill-0")).toContainText(
      "helm-release-management",
      { timeout: 30_000 },
    );
    await expect(page.getByTestId("template-form-skill-0")).toContainText("kagent");
    await expect(page.getByTestId("template-form-skill-1")).toContainText("prometheus-observability");
  });

  await test.step("3. a referenced template warns about everyone it serves", async () => {
    await loadPage(page, "/skills-catalog");
    await page.getByTestId("skills-catalog-manage").click();
    await selectOption(page, "agent-skills-agent-picker", "kagent/shared-brain-fast");
    const warning = page.getByTestId("agent-skills-shared-template");
    await expect(warning).toContainText("Template shared-brain is shared by 3 agents");
    // Looked at, not changed: the shared-template blast radius is the dialog's
    // message, and this journey does not need to pull that trigger.
    await page.getByRole("button", { name: "Close" }).click();
    await expect(page.getByTestId("agent-skills-dialog")).toHaveCount(0);
  });

  await test.step("4. managing an agent pre-checks its catalog skills, and unchecking removes", async () => {
    await page.getByTestId("skills-catalog-manage").click();
    await selectOption(page, "agent-skills-agent-picker", "kagent/k8s-agent-7f3a91c");

    // The catalog browser also lives on the page behind the dialog, so every
    // assertion here scopes to the dialog's own browser.
    const dialog = page.getByTestId("agent-skills-dialog");
    const helm = dialog.getByTestId("skills-catalog-check-helm-release-management");
    await expect(helm).toBeChecked({ timeout: 30_000 });
    await expect(dialog.getByTestId("skills-catalog-check-k8s-troubleshooting")).toBeChecked();

    await helm.uncheck();
    await dialog.getByTestId("skills-catalog-apply").click();
    await expect(page.getByText("Template k8s-agent-7f3a91c saved")).toBeVisible();

    // The template's own page says what survived: the custom Git skill the
    // catalog never owned, and the kagent skill still there. In-context again,
    // for the same reason as step 2.
    await page.getByTestId("nav-agents").click();
    await page.getByRole("tab", { name: /Templates/ }).click();
    await page.getByTestId("template-link-k8s-agent-7f3a91c").click();
    await page.waitForURL(new RegExp(`/agent-templates/kagent/k8s-agent-7f3a91c`));
    await expect(page.getByTestId("template-form-skill-0")).toContainText("k8s-troubleshooting", {
      timeout: 30_000,
    });
    await expect(page.getByTestId("template-form-skill-name-1")).toHaveText("incident-review");
    await expect(page.getByTestId("template-form-skill-2")).toHaveCount(0);
  });

  await test.step("5. a failed agents read is named, not dressed up as an empty list", async () => {
    // The catalog itself is config delivered with the page, not a read — it
    // renders even when the control plane does not answer.
    await loadPage(page, "/skills-catalog", { scenario: "error" });
    await expect(page.getByTestId("skills-catalog-card-k8s-troubleshooting")).toBeVisible({
      timeout: 30_000,
    });
    await page.getByTestId("skills-catalog-manage").click();
    await expect(page.getByTestId("agent-skills-agents-error")).toBeVisible({ timeout: 30_000 });
    await page.getByRole("button", { name: "Close" }).click();
  });

  await test.step("6. an install with no agents offers nothing to add to", async () => {
    await loadPage(page, "/skills-catalog", { scenario: "empty" });
    await expect(page.getByTestId("skills-catalog-card-k8s-troubleshooting")).toBeVisible({
      timeout: 30_000,
    });
    await page.getByTestId("skills-catalog-manage").click();
    await expect(page.getByTestId("agent-skills-agents-error")).toHaveCount(0);
    await page.getByTestId("agent-skills-agent-picker").click();
    // antd's empty state renders twice — an SVG title and the description.
    await expect(page.locator(".ant-empty-description")).toBeVisible();
  });
});
