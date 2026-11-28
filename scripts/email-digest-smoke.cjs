const { chromium } = require("playwright");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");

(async () => {
  const base = process.argv[2];
  assert(base, "Email fixture URL required");
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const width of [900, 680, 390, 320]) {
      const page = await browser.newPage({ viewport: { width, height: 1000 }, javaScriptEnabled: false });
      const external = [];
      page.on("request", request => {
        if (new URL(request.url()).origin !== new URL(base).origin) external.push(request.url());
      });
      await page.goto(base);
      assert.equal(await page.title(), "Docklight image update digest");
      const logo = page.getByAltText("Docklight logo");
      assert(await logo.evaluate(node => node.complete && node.naturalWidth > 0), "logo did not load");
      assert.equal(await page.getByRole("heading", { level: 2 }).count(), 2, "host groups missing");
      assert.equal(await page.getByRole("heading", { level: 3 }).count(), 3, "stack/standalone groups missing");
      assert((await page.locator("body").innerText()).includes("web-1, web-2"));
      assert((await page.locator("body").innerText()).includes("2 image updates"));
      assert(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), `email overflow at ${width}px`);
      const bounds = await page.locator("table").evaluateAll(tables => tables.map(table => table.getBoundingClientRect().toJSON()));
      assert(bounds.every(box => box.left >= 0 && box.right <= width), `email table escaped viewport at ${width}px`);
      assert(bounds[1].width <= 680, "digest must remain readable on desktop");
      assert.deepEqual(external, [], "email preview fetched external resources");
      const screenshots = process.env.DOCKLIGHT_UI_SCREENSHOT_DIR;
      if (screenshots) {
        await fs.mkdir(screenshots, { recursive: true });
        await page.screenshot({ path: path.join(screenshots, `email-digest-${width}.png`), fullPage: true });
      }
      await page.close();
    }
    console.log("PASS branded multi-host email layout, inline logo, long values and mobile bounds without JavaScript");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
