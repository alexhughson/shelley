import { test, expect } from "@playwright/test";
import type { Model } from "../src/types";
import { reasoningMetadata, testWorkingDirectory } from "./helpers";

// The unified model + effort picker (ChatStatusContent -> ModelPicker.vue) is
// built on PrimeVue <Select>. It renders on the new-conversation screen. Here
// we exercise the PrimeVue-specific open/select behavior, the inline
// reasoning-effort pill row, the pinned "Manage models…" footer action, and
// persistence of the chosen model + effort to localStorage.
test.describe("Model picker (PrimeVue)", () => {
  test("opens, lists models, selecting one persists, footer opens manage modal", async ({
    page,
  }) => {
    test.setTimeout(60000);

    await page.goto("/new");
    await page.waitForLoadState("domcontentloaded");

    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });

    // Open the overlay.
    await picker.click();
    const panel = page.locator(".model-picker-panel");
    await expect(panel).toBeVisible();

    // At least one model is offered and the footer actions are present.
    const options = panel.locator(".p-select-option");
    expect(await options.count()).toBeGreaterThanOrEqual(1);
    const manageBtn = panel.getByRole("button", { name: "Manage models…" });
    await expect(manageBtn).toBeVisible();
    await expect(panel.getByRole("button", { name: "Refresh" })).toBeVisible();

    // In a single-source install, no source sub-labels are rendered.
    await expect(panel.locator(".model-picker-option-source")).toHaveCount(0);

    // Pick the first model -> its label shows in the trigger and the raw model
    // id (not the pretty label) persists to localStorage.
    const firstName = (await options
      .first()
      .locator(".model-picker-option-name")
      .textContent())!.trim();
    await options.first().click();
    await expect(panel).toBeHidden();
    await expect(picker.locator(".model-picker-value-name")).toHaveText(firstName);
    expect(await page.evaluate(() => localStorage.getItem("shelley_selected_model"))).toBe(
      "predictable",
    );

    // The footer action opens the manage-models modal.
    await picker.click();
    await expect(panel).toBeVisible();
    await panel.getByRole("button", { name: "Manage models…" }).click();
    await expect(page.getByRole("dialog")).toBeVisible();
  });

  test("keeps model and directory inline when they fit, then wraps when needed", async ({
    page,
  }) => {
    // Pin the directory so layout doesn't depend on the length of the
    // server's checkout path (which varies across CI agents and wraps the
    // Dir chip onto its own line when long).
    await page.addInitScript(() => localStorage.setItem("shelley_selected_cwd", "/tmp/e2e-dir"));
    await page.setViewportSize({ width: 412, height: 915 });
    await page.goto("/new");

    const fieldTops = () =>
      page.evaluate(() => ({
        model: document.querySelector(".status-field-model")!.getBoundingClientRect().top,
        cwd: document.querySelector(".status-field-cwd")!.getBoundingClientRect().top,
      }));

    let tops = await fieldTops();
    expect(Math.abs(tops.model - tops.cwd)).toBeLessThan(2);

    await page.setViewportSize({ width: 320, height: 700 });
    tops = await fieldTops();
    expect(Math.abs(tops.model - tops.cwd)).toBeGreaterThan(2);
  });

  test("does not focus model search when opened on mobile", async ({ page }) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/new");

    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });
    await picker.click();

    const searchbox = page.locator(".model-picker-panel").getByRole("searchbox");
    await expect(searchbox).toBeVisible();
    await expect(searchbox).not.toBeFocused();
  });

  test("effort pills select a level, persist it, and keep the popover open", async ({ page }) => {
    test.setTimeout(60000);

    await page.goto("/new");
    await page.waitForLoadState("domcontentloaded");

    // Reset persisted level so assertions are deterministic across workers.
    await page.evaluate(() => localStorage.removeItem("shelley.thinkingLevel.v2"));
    await page.reload();
    await page.waitForLoadState("domcontentloaded");

    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });
    await picker.click();
    const panel = page.locator(".model-picker-panel");
    await expect(panel).toBeVisible();

    // Missing exact metadata retains the standard levels through xhigh, never max.
    const pills = panel.locator(".model-picker-effort-pill");
    await expect(pills).toHaveText(["auto", "off", "minimal", "low", "medium", "high", "xhigh"]);

    // Pick "high" -> persists, popover stays open, trigger shows the suffix.
    await pills.filter({ hasText: /^high$/ }).click();
    await expect(panel).toBeVisible();
    expect(await page.evaluate(() => localStorage.getItem("shelley.thinkingLevel.v2"))).toBe(
      "high",
    );
    await expect(pills.filter({ hasText: /^high$/ })).toHaveAttribute("aria-checked", "true");

    // Close the popover; the trigger reflects the effort and keeps it after reload.
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(picker.locator(".model-picker-value-effort")).toHaveText("· high");
    await page.reload();
    await expect(picker.locator(".model-picker-value-effort")).toHaveText("· high");
  });

  test("shows recent combinations, hidden while searching", async ({ page, request }) => {
    test.setTimeout(60000);

    const response = await request.post("/api/conversations/new", {
      data: {
        message: "echo recent model picker",
        model: "predictable",
        cwd: testWorkingDirectory(),
        conversation_options: { thinking_level: "high" },
      },
    });
    expect(response.ok()).toBeTruthy();
    const { conversation_id: conversationId } = await response.json();

    // The suite shares a conversation DB across workers. Other tests can
    // contribute a more popular recent model/effort combination, so keep
    // this page's picker history scoped to the conversation we just created.
    // Keep the stream connected for the picker, but ignore list patches
    // that would reintroduce other tests' conversations after the snapshot.
    await page.addInitScript(() => {
      const NativeEventSource = window.EventSource;
      window.EventSource = class extends NativeEventSource {
        constructor(url: string | URL, options?: EventSourceInit) {
          super(url, options);
          this.addEventListener("message", (event) => {
            const data = JSON.parse(event.data) as { conversation_list_patch?: unknown };
            if (data.conversation_list_patch) event.stopImmediatePropagation();
          });
        }
      };
    });
    await page.route("**/api/conversations/snapshot", async (route) => {
      const snapshotResponse = await route.fetch();
      const snapshot = (await snapshotResponse.json()) as {
        conversations: Array<{ conversation_id: string }>;
        hash: string;
      };
      const seed = snapshot.conversations.find(
        (conversation) => conversation.conversation_id === conversationId,
      );
      if (!seed) throw new Error("recent model picker conversation missing from snapshot");
      await route.fulfill({
        response: snapshotResponse,
        json: { ...snapshot, conversations: [seed] },
      });
    });

    await page.addInitScript(() => localStorage.setItem("shelley.thinkingLevel.v2", "high"));
    await page.goto("/new");
    const picker = page.locator(".model-picker.p-select");
    await expect(picker).toBeVisible({ timeout: 10000 });
    await picker.click();

    const panel = page.locator(".model-picker-panel");
    await expect(panel.locator(".model-picker-group-label")).toHaveCount(0);

    const recentRow = panel.locator(".p-select-option:has(.model-picker-option-effort)");
    const modelRow = panel.locator(".p-select-option:not(:has(.model-picker-option-effort))");
    await expect(modelRow.locator(".model-picker-option-name")).toHaveText("predictable");
    await expect(modelRow.locator(".model-picker-option-check")).toBeVisible();

    await panel.locator(".model-picker-effort-pill").filter({ hasText: /^low$/ }).click();

    await expect(panel.locator(".model-picker-group-label")).toHaveText("Recent");
    await expect(panel.locator(".model-picker-group-divider")).toBeVisible();
    await expect(recentRow.locator(".model-picker-option-name")).toHaveText("predictable");
    await expect(recentRow.locator(".model-picker-option-effort")).toHaveText("high");
    await expect(recentRow.locator(".model-picker-option-recent")).toBeVisible();
    await expect(modelRow.locator(".model-picker-option-recent")).toHaveCount(0);
    await expect(recentRow.locator(".model-picker-option-check")).toHaveCount(0);
    await expect(panel.locator(".p-select-option-group").first()).toHaveCSS(
      "background-color",
      "rgba(0, 0, 0, 0)",
    );
    await expect(recentRow).not.toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
    await expect(recentRow).toHaveCSS("border-top-width", "1px");
    const nameBox = await recentRow.locator(".model-picker-option-name").boundingBox();
    const effortPill = recentRow.locator(".model-picker-option-effort");
    const effortBox = await effortPill.boundingBox();
    expect(effortBox!.x).toBeGreaterThan(nameBox!.x + nameBox!.width);
    await expect(effortPill).not.toHaveCSS("background-color", "rgba(0, 0, 0, 0)");

    await recentRow.hover();
    await expect(recentRow).not.toHaveClass(/p-focus/);

    await panel.getByRole("searchbox").fill("predictable");
    await expect(panel.locator(".model-picker-group-label")).toHaveCount(0);
    await expect(panel.locator(".p-select-option-group:visible")).toHaveCount(0);

    await panel.getByRole("searchbox").fill("");
    await expect(panel.locator(".model-picker-group-label")).toBeVisible();

    await recentRow.click();
    await expect(panel).toBeHidden();
    await expect(picker.locator(".model-picker-value-effort")).toHaveText("· high");
  });

  for (const [name, metadata] of [
    ["missing metadata", {}],
    ["support only", { supports_reasoning: true }],
    ["empty levels", { supports_reasoning: true, reasoning_levels: [] }],
    ["known default without levels", { default_reasoning_level: "high" }],
  ] satisfies [string, Partial<Model>][]) {
    test(`${name} retains standard choices and a stored concrete effort`, async ({ page }) => {
      await reasoningMetadata(page, metadata);
      await page.addInitScript(() => localStorage.setItem("shelley.thinkingLevel.v2", "xhigh"));
      await page.goto("/new");
      const picker = page.locator(".model-picker.p-select");
      await picker.click();
      const pills = page.locator(".model-picker-panel .model-picker-effort-pill");
      const levels = ["off", "minimal", "low", "medium", "high", "xhigh"];
      await expect(pills).toHaveText(
        metadata.default_reasoning_level ? levels : ["auto", ...levels],
      );
      await expect(pills.filter({ hasText: /^xhigh$/ })).toHaveAttribute("aria-checked", "true");
      await expect(picker.locator(".model-picker-value-effort")).toHaveText("· xhigh");
      expect(await page.evaluate(() => localStorage.getItem("shelley.thinkingLevel.v2"))).toBe(
        "xhigh",
      );
      await page.keyboard.press("Escape");
      await page.getByTestId("message-input").fill("/model ");
      const suggestions = page.getByTestId("model-arg-menu").locator(".slash-command-name");
      await expect(suggestions).toHaveText(["predictable", ...levels]);
    });
  }

  for (const reasoning_levels of [undefined, []]) {
    test(`known default is selected with ${reasoning_levels ? "empty" : "omitted"} levels`, async ({
      page,
    }) => {
      await reasoningMetadata(page, { default_reasoning_level: "medium", reasoning_levels });
      await page.goto("/new");
      await page.locator(".model-picker.p-select").click();
      const pills = page.locator(".model-picker-panel .model-picker-effort-pill");
      await expect(pills).toHaveText(["off", "minimal", "low", "medium", "high", "xhigh"]);
      await expect(pills.filter({ hasText: /^medium$/ })).toHaveAttribute("aria-checked", "true");
    });
  }

  for (const [modelDefault, expected, selected] of [
    ["high", ["auto", "low", "max"], "auto"],
    ["max", ["low", "max"], "max"],
  ] as const) {
    test(`default ${modelDefault} is selectable only when advertised`, async ({ page }) => {
      await reasoningMetadata(page, {
        supports_reasoning: true,
        reasoning_levels: ["low", "max"],
        default_reasoning_level: modelDefault,
      });
      await page.goto("/new");
      await page.locator(".model-picker.p-select").click();
      const pills = page.locator(".model-picker-panel .model-picker-effort-pill");
      await expect(pills).toHaveText([...expected]);
      await expect(pills.filter({ hasText: new RegExp(`^${selected}$`) })).toHaveAttribute(
        "aria-checked",
        "true",
      );
      await page.keyboard.press("Escape");
      await page.getByTestId("message-input").fill("/model ");
      await expect(page.getByTestId("model-arg-menu").locator(".slash-command-name")).toHaveText([
        "predictable",
        "low",
        "max",
      ]);
    });
  }

  test("unsupported reasoning has no effort pills even with contradictory metadata", async ({
    page,
  }) => {
    await reasoningMetadata(page, {
      supports_reasoning: false,
      reasoning_levels: ["high"],
      default_reasoning_level: "high",
    });
    await page.goto("/new");
    await page.locator(".model-picker.p-select").click();
    await expect(page.locator(".model-picker-panel .model-picker-effort-pill")).toHaveCount(0);
    await page.keyboard.press("Escape");
    await page.getByTestId("message-input").fill("/model ");
    await expect(page.getByTestId("model-arg-menu").locator(".slash-command-name")).toHaveText([
      "predictable",
    ]);
  });
});
