const { chromium } = require("playwright");
const assert = require("node:assert/strict");

(async () => {
  const base = process.argv[2];
  assert(base, "UI fixture URL required");
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const javaScriptEnabled of [false, true]) {
      for (const width of [1440, 900, 390, 320]) {
        const context = await browser.newContext({ viewport: { width, height: 900 }, javaScriptEnabled });
        const page = await context.newPage();
        const errors = [];
        page.on("pageerror", error => errors.push(error.message));
        await page.goto(`${base}/login`);
        await page.getByLabel("Admin password").fill("pw");
        await page.getByRole("button", { name: "Login", exact: true }).click();
        const group = page.locator('details.stack-group[data-project="web"]');
        const summary = group.locator(":scope > summary");
        assert.equal(await group.evaluate(node => node.open), false);
        assert((await summary.innerText()).toLowerCase().includes("1 service with updates"));
        assert.equal(await summary.locator(".badge-update-available").isVisible(), true);
        await summary.focus();
        await page.keyboard.press("Enter");
        const rows = group.locator("article.has-image-update");
        assert.equal(await rows.count(), 2);
        const detail = rows.first().locator("details.image-update-detail");
        await detail.locator("summary").focus();
        await page.keyboard.press("Enter");
        assert.equal(await detail.evaluate(node => node.open), true);
        assert((await detail.innerText()).includes("Deployed: sha256:"));
        assert((await detail.innerText()).includes("Registry: sha256:"));
        assert((await detail.innerText()).includes("Checked:"));
        assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `overflow at ${width}px`);
        if (javaScriptEnabled) {
          await page.goto(`${base}/?host=nas&job=update-view`);
          if (!(await group.evaluate(node => node.open))) await summary.click();
          await detail.locator("summary").focus();
          await page.keyboard.press("Enter");
          await page.waitForFunction(() => document.querySelector("[data-operation-message]").textContent.includes("Inventory refresh paused"));
          assert.equal(await detail.locator("summary").evaluate(node => node === document.activeElement), true, "refresh stole image-detail focus");
          await page.locator('.host-form select').focus();
          await page.waitForFunction(() => document.querySelector("[data-operation-message]").textContent.includes("Inventory refreshed"));
          assert.equal(await detail.evaluate(node => node.open), true, "refresh collapsed image details");
        }
        await page.goto(`${base}/?host=nas&view=cleanup`);
        assert.equal(await page.locator("#images .badge-update-available").count(), 1);
        assert.deepEqual(errors, []);
        await context.close();
      }
    }
    console.log("PASS image-update badges, service counts, keyboard details, image status and responsive layout with/without JavaScript");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
