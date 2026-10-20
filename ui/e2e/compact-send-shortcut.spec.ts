import { expect, test, type Page } from "@playwright/test";
import { createConversationViaAPIWithDetails } from "./helpers";

// Mod+Enter (⌘ on mac, Ctrl elsewhere) compacts and sends regardless of the
// context threshold. Playwright reports a Linux platform, so Ctrl is the mod.
async function openConversation(
  page: Page,
  request: Parameters<typeof createConversationViaAPIWithDetails>[0],
) {
  const { conversationId, slug } = await createConversationViaAPIWithDetails(
    request,
    "echo: compact shortcut seed",
  );
  let compactRequests = 0;
  const chatRequests: Array<{ message?: string; queue?: boolean }> = [];
  await page.route("**/api/conversations/distill-new-generation", async (route) => {
    compactRequests++;
    await route.fulfill({ json: { conversation_id: conversationId, current_generation: 2 } });
  });
  await page.route(`**/api/conversation/${conversationId}/chat`, async (route) => {
    chatRequests.push(route.request().postDataJSON());
    await route.fulfill({ json: {} });
  });
  await page.route("**/api/stream2*", (route) => route.abort());
  await page.goto(`/c/${slug}`);
  await expect(page.getByTestId("message-input")).toBeVisible({ timeout: 30_000 });
  await expect(page.getByTestId("send-button")).toHaveAttribute("aria-label", "Send message");
  return { compactRequests: () => compactRequests, chatRequests };
}

test.describe("compact-send-shortcut", () => {
  test.use({ viewport: { width: 1280, height: 800 }, isMobile: false, hasTouch: false });

  test("Ctrl+Enter compacts and queues", async ({ page, request }) => {
    const { compactRequests, chatRequests } = await openConversation(page, request);
    const input = page.getByTestId("message-input");
    await input.fill("queued via shortcut");
    await input.press("Control+Enter");

    await expect.poll(compactRequests).toBe(1);
    await expect
      .poll(() => chatRequests)
      .toEqual([{ message: "queued via shortcut", model: "predictable", queue: true }]);
    await expect(input).toHaveValue("");
  });

  test("holding Ctrl previews compact-and-send on the send button", async ({ page, request }) => {
    await openConversation(page, request);
    const input = page.getByTestId("message-input");
    const sendButton = page.getByTestId("send-button");
    await input.fill("hold to preview");

    await page.keyboard.down("Control");
    await expect(sendButton).toHaveAttribute("aria-label", "Compact and send");
    await page.keyboard.up("Control");
    await expect(sendButton).toHaveAttribute("aria-label", "Send message");

    // Commands never compact, even with the mod held.
    await input.fill("/model predictable");
    await page.keyboard.down("Control");
    await expect(sendButton).toHaveAttribute("aria-label", "Send message");
    await page.keyboard.up("Control");
  });

  test("Ctrl+click on send compacts and queues", async ({ page, request }) => {
    const { compactRequests, chatRequests } = await openConversation(page, request);
    const input = page.getByTestId("message-input");
    await input.fill("clicked via mod");
    await page.getByTestId("send-button").click({ modifiers: ["Control"] });

    await expect.poll(compactRequests).toBe(1);
    await expect
      .poll(() => chatRequests)
      .toEqual([{ message: "clicked via mod", model: "predictable", queue: true }]);
  });

  test("menu shows the shortcut hint", async ({ page, request }) => {
    await openConversation(page, request);
    await page.getByTestId("message-input").fill("hint");
    await page.getByTestId("send-options-button").click();
    await expect(page.getByTestId("compact-and-send-option")).toContainText("Ctrl+Enter");
  });
});
