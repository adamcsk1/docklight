const { chromium } = require("playwright");
const assert = require("node:assert/strict");

(async () => {
  const base = process.argv[2];
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const javaScriptEnabled of [false, true]) {
      const context = await browser.newContext({ javaScriptEnabled });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      let busy = false;
      try {
        await page.goto(`${base}/login`);
        await page.getByLabel("Admin password").fill("pw");
        await page.getByRole("button", { name: "Login", exact: true }).click();
        await page.goto(`${base}/?host=pi`);
        await page.request.post(`${base}/test-verification-busy`);
        busy = true;
        await page.locator('details[data-project="web"] > summary').click();
        const actionRequest = page.waitForRequest(request => request.url() === `${base}/action` && request.method() === "POST");
        const actionNavigation = page.waitForURL(url => url.searchParams.get("job") === "presentation-job" || url.searchParams.get("notice") === "maintenance-busy");
        const actionClick = page.getByRole("form", { name: "Actions for web-stopped", exact: true }).getByRole("button", { name: "Start", exact: true }).click();
        await actionRequest;
        await page.request.post(`${base}/test-action-entered`);
        const actionState = await Promise.race([
          actionNavigation.then(() => "completed"),
          page.waitForTimeout(100).then(() => "pending"),
        ]);
        assert.equal(actionState, "pending", "action did not wait for verification");
        await page.request.post(`${base}/test-verification-idle`);
        busy = false;
        await actionClick;
        await actionNavigation;
        assert.equal(page.url(), `${base}/?host=pi&job=presentation-job`);
        assert.equal(await page.locator("[data-inventory]").count(), 1);
        assert.equal(await page.getByLabel("Docker host").inputValue(), "pi");
        assert(!(await page.locator("[data-ui-status]").textContent()).includes("No operation was submitted."));
        if (javaScriptEnabled) {
          await page.request.post(`${base}/test-verification-busy`);
          busy = true;
          let retries = 0;
          const fourRetries = new Promise(resolve => page.on("response", response => {
            if (response.url().includes("/inventory?") && response.status() === 503 && ++retries === 4) resolve();
          }));
          await page.goto(`${base}/?host=pi&job=presentation-job`);
          await Promise.race([fourRetries, page.waitForTimeout(25_000).then(() => { throw new Error("busy inventory stopped retrying"); })]);
          assert((await page.locator("[data-ui-status]").textContent()).includes("Keeping current inventory"));
          assert.equal(await page.locator("[data-inventory]").count(), 1);
          await page.evaluate(() => window.busyPageMarker = "unchanged");
          await page.request.post(`${base}/test-verification-idle`);
          busy = false;
          await page.waitForFunction(() => document.querySelector("[data-ui-status]").textContent.includes("Inventory refreshed."));
          assert.equal(await page.evaluate(() => window.busyPageMarker), "unchanged");
        }
        assert.deepEqual(errors, []);
      } finally {
        if (busy) await page.request.post(`${base}/test-verification-idle`);
        await context.close();
      }
    }
    console.log("PASS busy verification: actions wait then submit once, dashboard retained, inventory retries and recovers without navigation");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
