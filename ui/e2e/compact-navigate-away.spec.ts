import { expect, test, type Page } from "@playwright/test";
import { createConversationViaAPIWithDetails } from "./helpers";

test.use({ viewport: { width: 1280, height: 800 }, isMobile: false, hasTouch: false });

// Hold the compact response until the caller releases it.
async function holdCompaction(page: Page, conversationId: string) {
  let release!: () => void;
  const held = new Promise<void>((resolve) => (release = resolve));
  let requests = 0;
  await page.route("**/api/conversations/distill-new-generation", async (route) => {
    requests++;
    await held;
    await route.fulfill({ json: { conversation_id: conversationId, current_generation: 2 } });
  });
  return { release, requests: () => requests };
}

// Compaction runs server-side; the client only kicks it off. If the user moves
// to another conversation while the request is in flight, the response landing
// must not yank them back to the conversation they compacted.
test.describe("compact while navigating away", () => {
  test("stays on the newly selected conversation", async ({ page, request }) => {
    const source = await createConversationViaAPIWithDetails(request, "echo: compact source");
    const other = await createConversationViaAPIWithDetails(request, "echo: compact other");
    const compaction = await holdCompaction(page, source.conversationId);

    await page.goto(`/c/${source.slug}`);
    const input = page.getByTestId("message-input");
    await expect(input).toBeVisible({ timeout: 30_000 });
    await input.fill("/compact");
    await input.press("Enter");
    await expect.poll(compaction.requests).toBe(1);

    // SPA-navigate to the other conversation while compaction is in flight.
    const otherRow = page.locator(
      `.conversation-item[data-conversation-id="${other.conversationId}"]`,
    );
    await otherRow.click();
    await expect(otherRow).toHaveClass(/active/);
    await expect(page).toHaveURL(new RegExp(`/c/${other.slug}$`));

    // Once the body is delivered, everything downstream (api promise, handler,
    // Vue's pre-flush URL-sync watcher, replaceState) runs in microtasks, so
    // the next round-trip to the page sees the final URL.
    const response = page.waitForResponse("**/api/conversations/distill-new-generation");
    compaction.release();
    await (await response).finished();

    await expect(page).toHaveURL(new RegExp(`/c/${other.slug}$`));
    await expect(otherRow).toHaveClass(/active/);
  });
});

// "Compact and send" compacts, then queues the composed message. Both halves
// belong to the conversation the user was in when they clicked; navigating
// away mid-compaction must not deliver the message to the newly selected one.
test.describe("compact and send while navigating away", () => {
  test("queues the message on the compacted conversation", async ({ page, request }) => {
    const source = await createConversationViaAPIWithDetails(request, "echo: compact-send source");
    const other = await createConversationViaAPIWithDetails(request, "echo: compact-send other");
    const compaction = await holdCompaction(page, source.conversationId);
    const chatPosts: string[] = [];
    await page.route("**/api/conversation/*/chat", async (route) => {
      chatPosts.push(new URL(route.request().url()).pathname);
      await route.fulfill({ json: {} });
    });

    await page.goto(`/c/${source.slug}`);
    const input = page.getByTestId("message-input");
    await expect(input).toBeVisible({ timeout: 30_000 });
    await input.fill("echo: after compaction");
    await page.getByTestId("send-options-button").click();
    await page.getByTestId("compact-and-send-option").click();
    await expect.poll(compaction.requests).toBe(1);

    const otherRow = page.locator(
      `.conversation-item[data-conversation-id="${other.conversationId}"]`,
    );
    await otherRow.click();
    await expect(page).toHaveURL(new RegExp(`/c/${other.slug}$`));

    compaction.release();
    await expect.poll(() => chatPosts).toEqual([`/api/conversation/${source.conversationId}/chat`]);
  });
});
