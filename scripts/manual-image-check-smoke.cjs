const { chromium } = require("playwright");
const assert = require("node:assert/strict");

(async () => {
  const base = process.argv[2];
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const [width, javaScriptEnabled] of [[1440, false], [390, true]]) {
      const context = await browser.newContext({ viewport: { width, height: 900 }, javaScriptEnabled });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      await page.goto(`${base}/?host=nas&view=maintenance`);
      const button = page.getByRole("button", { name: "Check image updates now", exact: true });
      const confirmation = page.getByRole("checkbox", { name: /I confirm updating all agents and UIs/ });
      assert.equal(await confirmation.isChecked(), false);
      const [response] = await Promise.all([
        page.waitForResponse(response => response.url() === `${base}/image-updates/check` && response.request().method() === "POST"),
        button.click(),
      ]);
      assert.equal(response.status(), 303, "check must use alternate action, without update confirmation");
      await page.waitForURL(`${base}/?host=nas&view=maintenance`);
      assert.equal(await confirmation.isChecked(), false, "image check must not confirm upgrades");
      if (javaScriptEnabled) {
        const selected = page.locator('input[type="checkbox"][name="selected"]').first();
        await selected.uncheck();
        await selected.focus();
        await page.waitForFunction(() => document.querySelector("[data-image-check-status]")?.textContent.includes("2 current"), null, { timeout: 20_000 });
        assert.equal(await selected.isChecked(), false, "status polling reset selection");
        assert.equal(await selected.evaluate(input => input === document.activeElement), true, "status polling stole focus");
        assert.equal(await button.isEnabled(), true, "button did not recover after completion");
      } else {
        // Native submissions still work; status refresh is manual without JavaScript.
        for (let attempt = 0; attempt < 15; attempt++) {
          if ((await page.locator("[data-image-check-status]").textContent()).includes("2 current")) break;
          await page.waitForTimeout(500);
          await page.reload();
        }
      }
      assert((await page.locator("[data-image-check-status]").textContent()).includes("2 current"));
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await page.goto(`${base}/?host=nas`);
      const group = page.locator('details.stack-group[data-project="web"]');
      await group.locator(":scope > summary").click();
      assert.equal(await group.getByText("↑ Update available", { exact: true }).count(), 0, "badges did not refresh");
      assert.deepEqual(errors, []);
      await context.close();
    }
    console.log("PASS manual image checks: native/JS submission, base paths, no upgrade confirmation, progress/results, focus, mobile, refreshed badges");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
