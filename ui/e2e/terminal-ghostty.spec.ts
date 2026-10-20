import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  chooseTerminalMenuItem,
  closeTerminalsIn,
  createConversationViaAPIWithDetails,
  setPageTerminalRenderer,
  testWorkingDirectory,
} from "./helpers";

// ghostty-web is the default renderer. These cover what Shelley does on top
// of it; terminal-*.spec.ts cover the terminal panel on either renderer.

let cwd: string;
test.beforeEach(() => {
  cwd = mkdtempSync(join(testWorkingDirectory(), "terminal-ghostty-"));
});
test.afterEach(async ({ request }) => {
  await closeTerminalsIn(request, cwd);
  rmSync(cwd, { recursive: true, force: true });
});

/** Run a bash script in a new terminal; returns the visible terminal. */
async function runScript(page: Page, request: APIRequestContext, script: string[]) {
  writeFileSync(join(cwd, "t.sh"), script.join("\n") + "\n");
  const { conversationId } = await createConversationViaAPIWithDetails(
    request,
    "terminal ghostty test",
    { cwd },
  );
  await page.goto(`/c/${conversationId}`);
  await page.getByTestId("message-input").fill("!bash t.sh");
  await page.getByTestId("send-button").click();
  return page.locator('.terminal-panel-content [data-terminal-id][style*="display: block"]');
}

/** The terminal's text, via the panel menu's Insert items. */
function inserted(page: Page, what: "all output" | "visible screen") {
  const input = page.getByTestId("message-input");
  return async () => {
    await input.fill("");
    await chooseTerminalMenuItem(page, `Insert ${what} into input`);
    return input.inputValue();
  };
}

/** The color the canvas paints its empty bottom row with. */
function canvasBackground(terminal: ReturnType<Page["locator"]>) {
  return terminal.locator("canvas").evaluate((c: HTMLCanvasElement) => {
    const [r, g, b] = c.getContext("2d")!.getImageData(c.width / 2, c.height - 2, 1, 1).data;
    return `rgb(${r}, ${g}, ${b})`;
  });
}

const DARK = "rgb(26, 27, 38)";
const LIGHT = "rgb(248, 249, 250)";

/** What the theme menu does. */
async function setTheme(page: Page, theme: "dark" | "light") {
  await page.evaluate((t) => {
    localStorage.setItem("shelley-theme", t);
    document.documentElement.classList.toggle("dark", t === "dark");
  }, theme);
}

// ghostty-web can't change its palette in place: a theme switch reopens the
// terminal, which must get its screen back, keep talking to the same
// program, and leave focus where it was. The screen includes a gap, wide
// characters and a multi-code-point cluster, which Copy/Insert must keep.
test("survives theme switches and keeps Ctrl+V and Ctrl+Shift+C off paste and ^C", async ({
  page,
  request,
}) => {
  const terminal = await runScript(page, request, [
    "stty raw -echo",
    "printf 'ghostty-ready A\\033[24GB 漢字 👩‍💻\\r\\n'",
    "IFS= read -r -d q keys",
    "stty sane",
    "printf 'keys=%q\\n' \"$keys\"",
  ]);
  await expect(terminal.locator("canvas")).toHaveCount(1);
  await expect(terminal.locator(".xterm")).toHaveCount(0);
  const output = inserted(page, "all output");
  const screen = `ghostty-ready A${" ".repeat(8)}B 漢字 👩‍💻`;
  await expect.poll(output).toContain(screen);
  expect(await canvasBackground(terminal)).toBe(LIGHT);

  await page.evaluate(() => {
    const focused: string[] = [];
    Object.defineProperty(window, "__focused", { value: focused });
    document.addEventListener("focusin", (e) => {
      const inTerminal = (e.target as Element).closest(".terminal-panel-content");
      focused.push(inTerminal ? "terminal" : "elsewhere");
    });
  });
  await page.getByTestId("message-input").focus();
  let canvas = await terminal.locator("canvas").elementHandle();
  await setTheme(page, "dark");
  await expect.poll(() => canvas!.evaluate((el) => el.isConnected)).toBe(false);
  await expect.poll(() => canvasBackground(terminal)).toBe(DARK);
  await expect.poll(output).toContain(screen);
  expect(
    await page.evaluate(() => (window as unknown as { __focused: string[] }).__focused),
  ).not.toContain("terminal");

  // A focused terminal stays focused.
  const terminalInput = terminal.getByRole("textbox", { name: "Terminal input", exact: true });
  await terminalInput.focus();
  canvas = await terminal.locator("canvas").elementHandle();
  await setTheme(page, "light");
  await expect.poll(() => canvas!.evaluate((el) => el.isConnected)).toBe(false);
  await expect.poll(() => canvasBackground(terminal)).toBe(LIGHT);
  await expect(terminalInput).toBeFocused();

  await page.keyboard.press("Control+v");
  await page.keyboard.press("Control+Shift+c");
  await page.keyboard.press("q");
  await expect(page.locator(".terminal-panel-tab-success")).toHaveCount(1);
  await expect.poll(output).toMatch(/keys=\$'\\026'/);
});

// A hidden terminal can't be measured. Reopening it must keep its size, not
// resize the program to the 80x24 default.
test("a theme switch leaves a hidden terminal's size alone", async ({ page, request }) => {
  const terminal = await runScript(page, request, [
    "trap 'echo winch $(stty size)' WINCH",
    "echo ready $(stty size)",
    "while :; do sleep 0.1; done",
  ]);
  const output = inserted(page, "all output");
  await expect.poll(output).toMatch(/ready \d+ \d+/);
  // Other tests' terminals may still be running, global ones with a tab here.
  const id = await terminal.getAttribute("data-terminal-id");
  const first = page.locator(`.terminal-panel-content [data-terminal-id="${id}"]`);
  const firstTab = page.locator('.terminal-panel-tab[title="bash t.sh"]');
  const canvas = await first.locator("canvas").elementHandle();
  await page.getByRole("button", { name: "New terminal", exact: true }).click();
  await expect(page.locator(".terminal-panel-tab-active .terminal-panel-tab-running")).toHaveCount(
    1,
  );
  await expect(firstTab).not.toHaveClass(/terminal-panel-tab-active/);
  await expect(first).toBeHidden();

  await setTheme(page, "dark");
  await expect.poll(() => canvas!.evaluate((el) => el.isConnected)).toBe(false);
  await expect(first.locator("canvas")).toHaveCount(1);
  await firstTab.click();
  await expect(first).toBeVisible();
  await expect.poll(() => canvasBackground(first)).toBe(DARK);
  expect(await output()).not.toContain("winch");
});

test("a theme switch while ghostty-web loads still applies", async ({ page, request }) => {
  let release!: () => void;
  const released = new Promise<void>((r) => (release = r));
  let requested!: () => void;
  const wasRequested = new Promise<void>((r) => (requested = r));
  await page.route("**/ghostty-web.js", async (route) => {
    requested();
    await released;
    await route.continue();
  });
  const terminal = await runScript(page, request, ["printf 'ghostty-ready\\n'", "exec cat"]);
  await wasRequested;
  await setTheme(page, "dark");
  release();
  await expect.poll(inserted(page, "all output")).toContain("ghostty-ready");
  await expect.poll(() => canvasBackground(terminal)).toBe(DARK);
});

test("says why when ghostty-web fails to load, and xterm.js can take over", async ({
  page,
  request,
}) => {
  await page.route("**/ghostty-web.js", (route) => route.fulfill({ status: 404 }));
  const terminal = await runScript(page, request, ["echo xterm-took-over", "exec cat"]);
  await expect(terminal).toContainText("Couldn't open the terminal");
  await expect(page.locator(".terminal-panel-tab .terminal-panel-tab-error")).toHaveCount(1);

  await chooseTerminalMenuItem(page, "Render with xterm.js");
  await expect(page.locator(".terminal-panel-tab-running")).toBeVisible();
  await expect(terminal).not.toContainText("Couldn't open the terminal");
  await expect.poll(inserted(page, "all output")).toContain("xterm-took-over");
});

for (const [direction, delta] of [
  ["growing", -60],
  ["shrinking", 60],
] as const) {
  test(`fits the grid after a burst of resizes (${direction})`, async ({ page, request }) => {
    const terminal = await runScript(page, request, ["printf 'ghostty-ready\\n'", "exec cat"]);
    await expect.poll(inserted(page, "all output")).toContain("ghostty-ready");
    // Drag the divider one step per frame, as a hand does: faster than
    // ghostty-web's FitAddon accepts resizes.
    await page.evaluate(async (delta) => {
      const handle = document.querySelector(".terminal-panel-resize-handle")!;
      const box = handle.getBoundingClientRect();
      let clientY = box.y + box.height / 2;
      handle.dispatchEvent(new MouseEvent("mousedown", { clientY, bubbles: true }));
      for (let i = 0; i < 3; i++) {
        clientY += delta;
        document.dispatchEvent(new MouseEvent("mousemove", { clientY }));
        await new Promise(requestAnimationFrame);
      }
      document.dispatchEvent(new MouseEvent("mouseup"));
    }, delta);
    const slack = () =>
      terminal.evaluate((el) => {
        const host = el.firstElementChild!.getBoundingClientRect();
        return host.height - el.querySelector("canvas")!.getBoundingClientRect().height;
      });
    // Within one row (about 17px) and not overflowing.
    await expect.poll(slack).toBeGreaterThanOrEqual(0);
    await expect.poll(slack).toBeLessThan(20);
  });
}

test("keeps 10,000 lines of history, as xterm.js does", async ({ page, request }) => {
  await runScript(page, request, ["seq 1 10000", "exec cat"]);
  const output = inserted(page, "all output");
  await expect.poll(output).toContain("\n10000");
  expect(await output()).toMatch(/^1$/m);
});

// xterm.js keeps a scrolled-back view on the same lines when output arrives,
// and returns to the bottom on input; ghostty-web jumps to the bottom on
// output. Once the history is full, new output evicts the oldest lines,
// which moves the viewed ones up.
test("output leaves a scrolled-back terminal where it is, typing doesn't", async ({
  page,
  request,
}) => {
  // Wide, so fewer lines fill the history.
  await page.setViewportSize({ width: 1400, height: 900 });
  const terminal = await runScript(page, request, [
    "seq 1 30000",
    "while [ ! -e go ]; do sleep 0.05; done",
    "seq 30001 33000",
    "echo more-output",
    "exec cat",
  ]);
  const screen = inserted(page, "visible screen");
  await expect.poll(screen).toContain("30000");
  const box = (await terminal.locator("canvas").boundingBox())!;
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.wheel(0, -1500);
  let previous = "";
  const settled = async () => {
    const now = await screen();
    const same = now === previous;
    previous = now;
    return same;
  };
  await expect.poll(settled).toBe(true);
  expect(previous).not.toContain("30000");
  writeFileSync(join(cwd, "go"), "");
  await expect.poll(inserted(page, "all output")).toContain("more-output");
  expect(await screen()).toBe(previous);

  // Typing right after a wheel, before any scroll animation is over.
  await terminal.evaluate((el) => {
    el.querySelector("canvas")!.dispatchEvent(
      new WheelEvent("wheel", { deltaY: -300, bubbles: true, cancelable: true }),
    );
    el.querySelector("textarea")!.dispatchEvent(
      new KeyboardEvent("keydown", { key: "x", code: "KeyX", bubbles: true, cancelable: true }),
    );
  });
  await expect.poll(settled).toBe(true);
  expect(previous).toContain("more-output");
});

// Repeated lines can't tell positions apart; the whole screen can.
test("a view scrolled back among repeated lines stays put", async ({ page, request }) => {
  await page.setViewportSize({ width: 1400, height: 900 });
  const terminal = await runScript(page, request, [
    "yes repeat | head -n 30000",
    "for i in $(seq 2 $(stty size | cut -d' ' -f1)); do echo marker-$i; done",
    "while [ ! -e go ]; do sleep 0.05; done",
    "yes repeat | head -n 3000",
    "echo more-output",
    "exec cat",
  ]);
  const screen = inserted(page, "visible screen");
  await expect.poll(screen).toContain("marker-2\n");
  // One row back: a repeated line over the markers.
  await terminal.evaluate((el) =>
    el
      .querySelector("canvas")!
      .dispatchEvent(new WheelEvent("wheel", { deltaY: -1, deltaMode: 1, bubbles: true })),
  );
  await expect.poll(screen).toMatch(/^repeat\nmarker-2\n/);
  const viewed = await screen();
  writeFileSync(join(cwd, "go"), "");
  await expect.poll(inserted(page, "all output")).toContain("more-output");
  expect(await screen()).toBe(viewed);
});

test("xterm.js chosen while ghostty-web loads takes over when the load fails", async ({
  page,
  request,
}) => {
  let fail = () => {};
  const failed = new Promise<void>((resolve) => (fail = resolve));
  await page.route("**/ghostty-web.js", async (route) => {
    await failed;
    await route.fulfill({ status: 404 });
  });
  const terminal = await runScript(page, request, ["echo xterm-took-over", "exec cat"]);
  await chooseTerminalMenuItem(page, "Render with xterm.js");
  fail();
  await expect(page.locator(".terminal-panel-tab-running")).toBeVisible();
  await expect(terminal).not.toContainText("Couldn't open the terminal");
  await expect.poll(inserted(page, "all output")).toContain("xterm-took-over");
});

// Terminals sharing a ghostty emulator reuse each other's freed memory
// without clearing it.
test("a new terminal shows none of a closed one's text", async ({ page, request }) => {
  await runScript(page, request, [
    "seq -f 'closed-terminal %g zzzzzzzzzzzzzzzzzzzzzzzzzzzzzz' 3000",
  ]);
  const output = inserted(page, "all output");
  await expect.poll(output).toContain("closed-terminal 3000");
  const tab = page.locator('.terminal-panel-tab[title="bash t.sh"]');
  await tab.getByRole("button", { name: "Close terminal", exact: true }).click();
  await expect(tab).toHaveCount(0);

  writeFileSync(join(cwd, "t2.sh"), "seq 3000\nexec cat\n");
  await page.getByTestId("message-input").fill("!bash t2.sh");
  await page.getByTestId("send-button").click();
  await expect.poll(output).toContain("\n3000");
  expect(await output()).not.toContain("closed-terminal");
});

// Each terminal has an emulator of tens of MB, which ghostty-web's
// leftover document listeners would keep after the terminal is gone.
test("reopened and closed terminals free their emulators", async ({ page, request }) => {
  const terminal = await runScript(page, request, ["echo ghostty-ready", "exec cat"]);
  await expect.poll(inserted(page, "all output")).toContain("ghostty-ready");
  // Other tests' terminals come and go on the shared server, so watch the
  // emulators alive now and count how many of those are freed.
  const cdp = await page.context().newCDPSession(page);
  const watch = async () => {
    const group = { objectGroup: "emulators" };
    const proto = await cdp.send("Runtime.evaluate", {
      expression: "WebAssembly.Memory.prototype",
      ...group,
    });
    const { objects } = await cdp.send("Runtime.queryObjects", {
      prototypeObjectId: proto.result.objectId!,
      ...group,
    });
    await cdp.send("Runtime.callFunctionOn", {
      objectId: objects.objectId!,
      functionDeclaration:
        "function () { globalThis.watchedEmulators = this.map((m) => new WeakRef(m)); }",
      ...group,
    });
    await cdp.send("Runtime.releaseObjectGroup", group);
  };
  const freed = async () => {
    await cdp.send("HeapProfiler.collectGarbage");
    return page.evaluate(
      () =>
        (globalThis as unknown as { watchedEmulators: WeakRef<object>[] }).watchedEmulators.filter(
          (ref) => !ref.deref(),
        ).length,
    );
  };

  await watch();
  await setTheme(page, "dark");
  await expect.poll(() => canvasBackground(terminal)).toBe(DARK);
  await expect.poll(freed).toBeGreaterThan(0);

  await watch();
  const tab = page.locator('.terminal-panel-tab[title="bash t.sh"]');
  await tab.getByRole("button", { name: "Close terminal", exact: true }).click();
  await expect(tab).toHaveCount(0);
  await expect.poll(freed).toBeGreaterThan(0);
});

// On a phone, a drag scrolls back through output instead of focusing the
// terminal and summoning the keyboard.
// xterm.js scrolls on touch itself; ghostty-web doesn't, so Shelley does.
// Either way a drag scrolls as far as the finger moves: once, not twice.
for (const renderer of ["ghostty", "xterm"] as const) {
  test(`a touch drag scrolls back as far as the finger moves, without focusing (${renderer})`, async ({
    page,
    request,
  }) => {
    await setPageTerminalRenderer(page, renderer);
    const terminal = await runScript(page, request, [
      "stty size | cut -d' ' -f1 > rows",
      "seq 1 300",
      "exec cat",
    ]);
    const screen = inserted(page, "visible screen");
    await expect.poll(inserted(page, "all output")).toContain("\n300");
    const topLine = async () => parseInt(await screen(), 10);
    const before = await topLine();
    const box = (await terminal.boundingBox())!;
    const rowHeight = box.height / parseInt(readFileSync(join(cwd, "rows"), "utf8"), 10);
    const x = box.x + box.width / 2;
    const cdp = await page.context().newCDPSession(page);
    const touch = (type: "touchStart" | "touchMove" | "touchEnd", y?: number) =>
      cdp.send("Input.dispatchTouchEvent", {
        type,
        touchPoints: y === undefined ? [] : [{ x, y }],
      });
    // Finger moves down: content follows it, older lines come into view.
    const from = box.y + 10;
    const to = box.y + box.height - 10;
    await touch("touchStart", from);
    for (let y = from + 10; y <= to; y += 10) await touch("touchMove", y);
    await touch("touchEnd");
    const rows = (to - from) / rowHeight;
    await expect.poll(topLine).toBeLessThan(before - rows * 0.7);
    let last = NaN; // settled: the same top line twice running
    await expect.poll(async () => last === (last = await topLine())).toBe(true);
    expect(before - last).toBeLessThan(rows * 1.3);
    await expect(
      page.getByRole("textbox", { name: "Terminal input", exact: true }),
    ).not.toBeFocused();
  });
}

// While a program tracks the mouse (Claude Code, tmux, vim with mouse=a), a
// plain drag goes to it and Shift+drag selects, as in desktop terminals and
// xterm.js. Stock ghostty-web reports the Shift+drag too, which clears the
// selection before it can be copied.
test("Shift+drag selects and copies while the program tracks the mouse", async ({
  page,
  request,
}) => {
  await page.addInitScript(() => {
    const copied: string[] = [];
    Object.defineProperty(window, "__copied", { value: copied });
    navigator.clipboard.writeText = async (text: string) => void copied.push(text);
  });
  const terminal = await runScript(page, request, [
    "stty raw -echo",
    "printf '\\033[?1000h\\033[?1006h'",
    "printf 'select-me-please\\r\\n'",
    "exec cat >/dev/null",
  ]);
  await expect.poll(inserted(page, "all output")).toContain("select-me-please");

  const box = (await terminal.locator("canvas").boundingBox())!;
  await page.keyboard.down("Shift");
  await page.mouse.move(box.x + 2, box.y + 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2, { steps: 5 });
  await page.mouse.move(box.x + box.width - 2, box.y + box.height - 2, { steps: 5 });
  await page.mouse.up();
  await page.keyboard.up("Shift");
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __copied: string[] }).__copied))
    .toContainEqual(expect.stringContaining("select-me-please"));
});
