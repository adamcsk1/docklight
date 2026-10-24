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
      const skip = form.getByRole("checkbox", { name: "Skip offline devices", exact: true });
      const confirmation = form.getByRole("checkbox", { name: /I confirm updating all agents and UIs/ });
      const submit = form.getByRole("button");
      const labelSpacing = await confirmation.locator("..").evaluate(label => ({
        actual: parseFloat(getComputedStyle(label).paddingTop),
        expected: parseFloat(getComputedStyle(document.documentElement).fontSize) * 0.75,
      }));
      assert(labelSpacing.actual >= labelSpacing.expected, "maintenance confirmation label needs top padding");
      assert.equal(await skip.isChecked(), false);
      assert((await skip.boundingBox()).y < (await confirmation.boundingBox()).y, "skip checkbox must be above confirmation");
      await submit.click();
      assert.equal(await confirmation.evaluate(input => input.validity.valueMissing), true);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);

      // Regression: preflight failures must stay in Docklight, not become a blank text page.
      await page.request.post(`${base}/test-remote-offline`);
      await confirmation.check();
      const [errorResponse] = await Promise.all([
        page.waitForResponse(response => response.url() === `${base}/maintenance` && response.request().method() === "POST"),
        submit.click(),
      ]);
      assert.equal(errorResponse.status(), 409);
      await page.getByRole("heading", { name: "Request failed", exact: true }).waitFor();
      assert.equal(await page.locator(".topbar").isVisible(), true);
      assert.equal(await page.locator('link[rel="stylesheet"][href$="/static/app.css"]').count(), 1);
      assert((await page.getByRole("alert").textContent()).includes("remote: agent offline or unavailable"));
      await page.request.post(`${base}/test-remote-online`);
      await page.getByRole("link", { name: "Return to dashboard", exact: true }).click();
      await page.waitForURL(`${base}/?host=main`);
      assert.equal(await page.locator("[data-maintenance]").count(), 1);
      await page.getByLabel("Docker host").selectOption("remote");
      if (!javaScriptEnabled) await page.getByRole("button", { name: "Reconnect", exact: true }).click();
      await page.waitForURL(`${base}/?host=remote`);
      assert.equal(await page.locator("[data-maintenance]").count(), 0);
      assert.equal(await maintenanceLink.count(), 0);
      await page.getByLabel("Docker host").selectOption("main");
      if (!javaScriptEnabled) await page.getByRole("button", { name: "Reconnect", exact: true }).click();
      await page.waitForURL(`${base}/?host=main`);
      if (javaScriptEnabled) {
        await page.request.post(`${base}/test-remote-offline`);
        await skip.check();
        await confirmation.check();
        await submit.click();
        await page.waitForFunction(() => document.querySelector("[data-maintenance]")?.textContent.includes("Selected agents and UIs verified. Skipped offline devices: remote."), null, { timeout: 90_000 });
        assert.equal(await page.locator("[data-maintenance] details").count(), 2);
        assert.equal(await page.locator("[data-maintenance-skipped]").count(), 1);
        assert((await page.locator("[data-maintenance-skipped]").textContent()).includes("remote · no updates submitted"));

        await page.request.post(`${base}/test-remote-online`);
        await page.goto(`${base}/?host=main`);
        assert.equal(await skip.isChecked(), false, "a new run must not inherit skip by default");
        await confirmation.check();
        await submit.click();
        await page.waitForFunction(() => document.querySelector("[data-maintenance]")?.textContent.includes("All agents and UIs verified."), null, { timeout: 90_000 });
        assert.equal(await page.locator("[data-maintenance] details").count(), 4);
        const statusBottom = await page.locator("[data-maintenance] details").last().evaluate(item => item.getBoundingClientRect().bottom);
        const confirmationTop = await confirmation.locator("..").evaluate(item => item.getBoundingClientRect().top);
        const requiredGap = await page.evaluate(() => parseFloat(getComputedStyle(document.documentElement).fontSize));
        assert(confirmationTop - statusBottom >= requiredGap - 1, "maintenance confirmation too close to status details");
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
    console.log("PASS maintenance: styled errors, offline skipping, master-only controls, confirmation, progress, escaping, mobile, conflict blocking and logout");
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exitCode = 1; });
