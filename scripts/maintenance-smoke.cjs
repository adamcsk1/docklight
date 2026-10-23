const { chromium } = require("playwright");
const assert = require("node:assert/strict");

(async () => {
  const base = process.argv[2];
  const browser = await chromium.launch({ headless: true, channel: process.env.PLAYWRIGHT_BROWSER_CHANNEL || undefined });
  try {
    for (const [width, javaScriptEnabled] of [[1440, false], [320, false], [390, true]]) {
      const context = await browser.newContext({ viewport: { width, height: 900 }, javaScriptEnabled });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      assert.equal(await page.locator("[data-maintenance]").count(), 1);
      const maintenanceLink = page.getByRole("navigation").getByRole("link", { name: "maintenance", exact: true });
      assert.equal(await maintenanceLink.getAttribute("href"), "#maintenance");
      const assertPlacement = async () => {
        assert.equal(await page.locator("main section").last().getAttribute("id"), "maintenance");
        assert.equal(await page.locator("[data-inventory] [data-maintenance]").count(), 0);
        assert.equal(await page.locator("#maintenance").count(), 1);
      };
      await assertPlacement();
      await maintenanceLink.click();
      assert.equal(new URL(page.url()).hash, "#maintenance");
      const bounds = await page.locator("#maintenance").boundingBox();
      const header = await page.locator(".topbar").boundingBox();
      assert(bounds.y >= header.y + header.height, "maintenance heading hidden behind navigation");
      const form = page.getByRole("form", { name: "Update all agents and UIs", exact: true });
      await form.getByRole("button").click();
      assert.equal(await form.getByRole("checkbox").evaluate(input => input.validity.valueMissing), true);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await page.getByLabel("Docker host").selectOption("remote");
      if (!javaScriptEnabled) await page.getByRole("button", { name: "Reconnect", exact: true }).click();
      await page.waitForURL(`${base}/?host=remote`);
      assert.equal(await page.locator("[data-maintenance]").count(), 0);
      assert.equal(await maintenanceLink.count(), 0);
      await page.getByLabel("Docker host").selectOption("main");
      if (!javaScriptEnabled) await page.getByRole("button", { name: "Reconnect", exact: true }).click();
      await page.waitForURL(`${base}/?host=main`);
      if (javaScriptEnabled) {
        await form.getByRole("checkbox").check();
        await form.getByRole("button").click();
        await page.waitForFunction(() => document.querySelector("[data-maintenance]")?.textContent.includes("All agents and UIs verified."), null, { timeout: 90_000 });
        assert.equal(await page.locator("[data-maintenance] details").count(), 4);
        await assertPlacement();
        await maintenanceLink.click();
        assert.equal(new URL(page.url()).hash, "#maintenance");
        await page.locator("[data-maintenance] details summary").first().click();
        assert((await page.locator("[data-maintenance] pre").first().textContent()).includes("<script>not executable</script>"));
        assert.equal(await page.locator("script:not([src])").count(), 0);
        assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
        await page.request.post(`${base}/test-master-conflict`);
        await page.getByText("Master conflict — management blocked", { exact: true }).waitFor({ timeout: 30_000 });
        assert.equal(await page.locator("[data-inventory]").count(), 0);
        assert.equal(await page.locator('[action$="/action"]').count(), 0);
        assert.equal(await maintenanceLink.count(), 0);
        await page.getByRole("button", { name: "Logout", exact: true }).click();
        await page.waitForURL(`${base}/login`);
      }
      assert.deepEqual(errors, []);
      await context.close();
    }
    console.log("PASS maintenance: master-only controls, confirmation, progress, escaping, mobile, conflict blocking and logout");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
