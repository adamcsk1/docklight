// Optional development-only dependency; not required to build or run the application.
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
          inlineContent: document.querySelectorAll("script:not([src]),style,[style]").length,
        }));
        assert(metrics.contentWidth <= metrics.width, `page overflow at ${metrics.width}px: ${JSON.stringify(metrics)}`);
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
      if (width === 1440) {
        const session = (await context.cookies()).find(cookie => cookie.name === "docklight_session");
        assert(session, "login did not create a session");
        const assertSession = async (reason = "viewport/device switch") => {
          assert.equal(page.url(), `${base}/`, `${reason} sent authenticated user to login`);
          assert.equal(await page.title(), "Containers · Docklight");
          assert.equal((await context.cookies()).find(cookie => cookie.name === "docklight_session")?.value, session.value, "viewport/device switch changed the session cookie");
          await assertLayout();
        };
        for (const viewport of [{ width: 390, height: 844 }, { width: 1440, height: 1000 }, { width: 320, height: 700 }, { width: 900, height: 1000 }]) {
          await page.setViewportSize(viewport);
          await page.reload();
          await assertSession();
        }
        // DevTools device mode also changes user agent and mobile metrics.
        const desktopAgent = await page.evaluate(() => navigator.userAgent);
        const cdp = await context.newCDPSession(page);
        await cdp.send("Emulation.setUserAgentOverride", { userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36", platform: "Android" });
        await cdp.send("Emulation.setDeviceMetricsOverride", { width: 390, height: 844, deviceScaleFactor: 2, mobile: true });
        await page.reload();
        await assertSession();
        await cdp.send("Emulation.setUserAgentOverride", { userAgent: desktopAgent, platform: "" });
        await cdp.send("Emulation.setDeviceMetricsOverride", { width, height, deviceScaleFactor: 1, mobile: false });
        await cdp.detach();
        await page.setViewportSize({ width, height });
        await page.reload();
        await assertSession();
        // Reloading/restoring the login URL must not show an authenticated user a login form.
        await page.goto(`${base}/login`);
        await assertSession("login-URL restoration");
        console.log(`PASS ${label}: same-session resize, mobile user agent/device emulation, reload and login-URL restoration`);
      }
      await assertLayout();
      assert.equal(await page.locator("details.stack-group[open]").count(), 0);
      for (const group of await page.locator("details.stack-group").all()) await group.locator("summary").click();
      for (const name of ["Containers in docklight", "Containers in web", "Standalone containers", "Images", "Volumes", "Networks"]) assert.equal(await page.getByRole("table", { name, exact: true }).count(), 1);
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
      await webGroup.locator("summary").click();
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
    for (const storageBlocked of [false, true]) {
      const context = await browser.newContext({ viewport: { width: 1440, height: 1000 } });
      if (storageBlocked) {
        await context.addInitScript(() => {
          Object.defineProperty(window, "localStorage", { get() { throw new DOMException("Storage disabled for test", "SecurityError"); } });
        });
      }
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      page.on("console", message => { if (message.type() === "error") errors.push(message.text()); });
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      assert.equal(await page.locator("details.stack-group[open]").count(), 0, "new accordion state must be collapsed");
      const web = page.locator('details.stack-group[data-project="web"]');
      const agent = page.locator('details.stack-group[data-project="docklight"]');
      const standalone = page.locator('details.stack-group[data-project=""]');
      const prefix = new URL(base).pathname.replace(/\/$/, "");
      const key = (host, project) => "docklight:accordion:" + JSON.stringify([prefix, host, project]);
      const waitStored = (host, project, state) => page.waitForFunction(([key, state]) => localStorage.getItem(key) === state, [key(host, project), state]);
      if (!storageBlocked) {
        await page.evaluate(key => localStorage.setItem(key, "invalid-state"), key("local", "web"));
        await page.reload();
        assert.equal(await web.evaluate(group => group.open), false, "invalid saved state must stay collapsed");
      }
      await web.locator("summary").click();
      assert.equal(await web.evaluate(group => group.open), true);
      if (storageBlocked) {
        await page.reload();
        assert.equal(await web.evaluate(group => group.open), false);
        await web.locator("summary").press("Space");
        assert.equal(await web.evaluate(group => group.open), true, "blocked storage must not break native controls");
      } else {
        await waitStored("local", "web", "open");
        await page.reload();
        assert.equal(await web.evaluate(group => group.open), true, "expanded state was not restored");
        assert.equal(await agent.evaluate(group => group.open), false, "another stack inherited the expanded state");
        await web.locator("summary").click();
        await waitStored("local", "web", "closed");
        await page.reload();
        assert.equal(await web.evaluate(group => group.open), false, "collapsed state was not restored");
        await web.locator("summary").click();
        await waitStored("local", "web", "open");
        await page.getByLabel("Docker host").selectOption("pi");
        await page.getByRole("button", { name: "Connect", exact: true }).click();
        await page.waitForURL(`${base}/?host=pi`);
        assert.equal(await page.locator("details.stack-group[open]").count(), 0, "another host inherited saved stack state");
        await agent.locator("summary").click();
        await waitStored("pi", "docklight", "open");
        await page.getByLabel("Docker host").selectOption("local");
        await page.getByRole("button", { name: "Connect", exact: true }).click();
        await page.waitForURL(`${base}/?host=local`);
        assert.equal(await web.evaluate(group => group.open), true);
        assert.equal(await agent.evaluate(group => group.open), false);
        assert.equal(await standalone.evaluate(group => group.open), false);
        await standalone.locator("summary").click();
        await waitStored("local", "", "open");
        const session = (await context.cookies()).find(cookie => cookie.name === "docklight_session").value;
        for (const width of [390, 1440]) {
          await page.setViewportSize({ width, height: 1000 });
          await page.reload();
          assert.equal(await web.evaluate(group => group.open), true);
          assert.equal(await standalone.evaluate(group => group.open), true);
          assert.equal((await context.cookies()).find(cookie => cookie.name === "docklight_session")?.value, session);
        }
      }
      assert.deepEqual(errors, [], "accordion storage caused browser/CSP errors");
      await page.getByRole("button", { name: "Logout", exact: true }).click();
      await page.waitForURL(`${base}/login`);
      console.log(`PASS ${base}: accordion persistence ${storageBlocked ? "storage-disabled fallback" : "open/closed, reload, host/stack isolation and resize"}`);
      await context.close();
    }
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
