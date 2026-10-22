// Optional development-only dependency; the application has no JavaScript runtime.
const { chromium } = require("playwright");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");

(async () => {
  const base = process.argv[2];
  assert(base, "UI fixture URL required");
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const [width, height] of [[1440, 1000], [900, 1000], [390, 844], [320, 700]]) {
      const context = await browser.newContext({ viewport: { width, height }, javaScriptEnabled: false });
      const page = await context.newPage();
      const screenshots = process.env.DOCKLIGHT_UI_SCREENSHOT_DIR;
      const label = new URL(base).pathname.replaceAll("/", "") || "root";
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
      const assertLayout = async () => {
        const metrics = await page.evaluate(() => ({
          width: window.innerWidth,
          contentWidth: document.documentElement.scrollWidth,
          background: getComputedStyle(document.body).backgroundColor,
          font: getComputedStyle(document.body).fontFamily,
          inlineContent: document.querySelectorAll("script,style,[style]").length,
        }));
        assert(metrics.contentWidth <= metrics.width, `page overflow at ${width}px: ${JSON.stringify(metrics)}`);
        assert.equal(metrics.background, "rgb(8, 9, 11)");
        assert.equal(metrics.font, "monospace");
        assert.equal(metrics.inlineContent, 0);
      };
      await page.goto(`${base}/login`);
      await assertLayout();
      const loginPanel = await page.locator(".login-panel").boundingBox();
      assert(loginPanel.width <= 448 && loginPanel.x >= 0, "login panel no longer uses the original responsive card");
      if (screenshots) {
        await fs.mkdir(screenshots, { recursive: true });
        await page.screenshot({ path: path.join(screenshots, `${label}-${width}-login.png`) });
      }
      assert.equal(await page.locator('input[name="username"]').count(), 0);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      assert.equal(await page.title(), "Containers · Docklight");
      await assertLayout();
      for (const name of ["Containers in docklight", "Containers in web", "Standalone containers", "Images", "Volumes", "Networks"]) assert.equal(await page.getByRole("table", { name, exact: true }).count(), 1);
      assert.equal(await page.locator("details.stack-group[open]").count(), 3);
      const webGroup = page.locator('details.stack-group[aria-label="Compose stack web"]');
      assert((await webGroup.locator("summary").textContent()).includes("1 / 2 running"));
      assert.equal(await webGroup.getByRole("row").count(), 3);
      assert.equal(await page.getByText("UI activity and retained legacy history", { exact: true }).count(), 0);
      await webGroup.locator("summary").click();
      assert.equal(await webGroup.evaluate(group => group.open), false);
      assert.equal(await webGroup.getByRole("row").count(), 0);
      await webGroup.locator("summary").press("Space");
      assert.equal(await webGroup.evaluate(group => group.open), true);
      const row = name => page.getByRole("row").filter({ has: page.getByText(name, { exact: true }) });
      assert.equal(await row("web-running").getByRole("button", { name: "Delete stopped", exact: true }).isDisabled(), true);
      assert.equal(await row("docklight-agent").getByRole("button").count(), 1);
      assert.equal(await row("docklight-agent").getByRole("button", { name: "Update", exact: true }).count(), 1);
      assert.equal(await row("standalone").getByRole("button", { name: "Recreate", exact: true }).count(), 0);
      assert.equal(await row("used-volume").locator("form").count(), 0);
      assert.equal(await row("bridge").locator("form").count(), 0);
      if (screenshots) {
        await page.screenshot({ path: path.join(screenshots, `${label}-${width}.png`) });
      }
      // The host selector must work without JavaScript and carry through every action.
      await page.getByLabel("Docker host").selectOption("pi");
      await page.getByRole("button", { name: "Connect", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi`);
      await row("web-stopped").getByRole("button", { name: "Start", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      await assertLayout();
      assert((await page.getByLabel("Operation output").textContent()).includes("<script>not executable</script>"));
      // Required confirmation must stop an unchecked resource delete in the browser.
      const unused = row("unused-volume");
      await unused.getByRole("button", { name: "Delete", exact: true }).click();
      assert.equal(await unused.getByRole("checkbox").evaluate(input => input.validity.valueMissing), true);
      await unused.getByRole("checkbox").check();
      await unused.getByRole("button", { name: "Delete", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      await assertLayout();
      for (const section of ["containers", "images", "networks", "activity"]) {
        await page.getByRole("navigation").getByRole("link", { name: section, exact: true }).click();
        await page.waitForURL(`${base}/?host=pi&job=presentation-job#${section}`);
        const target = await page.locator(`#${section}`).boundingBox();
        const topbar = await page.locator(".topbar").boundingBox();
        assert(target.y >= topbar.y + topbar.height, `navigation hides ${section} heading`);
      }
      await page.getByRole("navigation").getByRole("link", { name: "volumes", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job#volumes`);
      const section = await page.locator("#volumes").boundingBox();
      const bar = await page.locator(".topbar").boundingBox();
      assert(section.y >= bar.y + bar.height, "sticky navigation hides the section heading");
      if (screenshots) await page.screenshot({ path: path.join(screenshots, `${label}-${width}-volumes.png`) });
      assert.deepEqual(errors, [], `browser errors at ${width}px`);
      await page.getByRole("button", { name: "Logout", exact: true }).click();
      await page.waitForURL(`${base}/login`);
      console.log(`PASS ${label}: ${width}x${height}, JavaScript disabled, stack groups/keyboard collapse/counts, no overflow/CSP errors, forms and host routing checked`);
      await context.close();
    }
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
