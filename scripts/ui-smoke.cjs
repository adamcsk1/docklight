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
      assert.equal(await page.locator('.host-form [data-build-component="agent"]').textContent(), "aaaaaaa");
      assert.equal(await page.locator('[data-build-component="agent"]').getAttribute("title"), "a".repeat(40));
      await page.locator('[data-build-component="agent"]').focus();
      assert.equal(await page.locator('[data-build-component="agent"]').evaluate(label => getComputedStyle(label, "::after").content), '"' + "a".repeat(40) + '"', "full commit unavailable on keyboard focus");
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
      assert.equal(await page.locator('#activity, [href="#activity"], .activity-list, .activity-item').count(), 0);
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
      assert.equal(await row("web-stopped").getByRole("button", { name: "Build", exact: true }).count(), 1);
      for (const name of ["web-running", "docklight-agent", "standalone"]) assert.equal(await row(name).getByRole("button", { name: "Build", exact: true }).count(), 0);
      assert((await webGroup.getByRole("row").filter({ has: page.getByText("web-running", { exact: true }) }).textContent()).includes("Build unavailable: Compose config unavailable"));
      assert.equal(await row("used-volume").locator("form").count(), 0);
      assert.equal(await row("bridge").locator("form").count(), 0);
      if (screenshots) {
        await page.screenshot({ path: path.join(screenshots, `${label}-${width}.png`) });
      }
      // The host selector must work without JavaScript and carry through every action.
      await page.getByLabel("Docker host").selectOption("pi");
      await page.getByRole("button", { name: "Reconnect", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi`);
      assert.equal(await page.locator('[data-build-component="agent"]').textContent(), "bbbbbbb", "host switch retained old agent commit");
      await webGroup.locator("summary").click();
      await row("web-stopped").getByRole("button", { name: "Start", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      await assertLayout();
      assert((await page.getByLabel("Operation output").textContent()).includes("<script>not executable</script>"));
      const title = await page.locator("#operation-title").boundingBox();
      const status = await page.locator("[data-operation-status]").boundingBox();
      const close = await page.getByRole("link", { name: "Close output", exact: true }).boundingBox();
      if (width >= 900) {
        assert(Math.abs(title.y + title.height / 2 - status.y - status.height / 2) <= 2, "operation title/status not vertically aligned");
        assert(status.x - title.x - title.width <= 25, "status pill separated from operation title");
        assert(close.x > status.x + status.width, "Close output not right of title/status");
      } else {
        assert(close.y >= Math.max(title.y + title.height, status.y + status.height), "mobile close control overlaps operation heading");
      }
      await page.getByRole("link", { name: "Close output", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job&output=hidden`);
      assert.equal(await page.getByLabel("Operation output").isVisible(), false);
      assert.equal(await page.getByRole("link", { name: "Show output", exact: true }).isVisible(), true);
      await webGroup.locator("summary").click();
      await row("web-stopped").getByRole("button", { name: "Build", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      assert.equal(await page.getByLabel("Operation output").isVisible(), true, "new operation must reopen output");
      // Required confirmation must stop an unchecked resource delete in the browser.
      const unused = row("unused-volume");
      await unused.getByRole("button", { name: "Delete", exact: true }).click();
      assert.equal(await unused.getByRole("checkbox").evaluate(input => input.validity.valueMissing), true);
      await unused.getByRole("checkbox").check();
      await unused.getByRole("button", { name: "Delete", exact: true }).click();
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      await assertLayout();
      for (const section of ["containers", "images", "networks"]) {
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
        await page.waitForURL(`${base}/?host=pi`);
        assert.equal(await page.locator("details.stack-group[open]").count(), 0, "another host inherited saved stack state");
        await agent.locator("summary").click();
        await waitStored("pi", "docklight", "open");
        await page.getByLabel("Docker host").selectOption("local");
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
    {
      const context = await browser.newContext({ viewport: { width: 390, height: 844 } });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      await page.waitForLoadState("load");
      // Hold the native navigation after feedback paints, then release the real form submission.
      const holdSubmission = () => page.evaluate(() => {
        window.nativeSubmit = HTMLFormElement.prototype.submit;
        window.pendingSubmissions = 0;
        window.pendingForm = null;
        HTMLFormElement.prototype.submit = function () { window.pendingForm = this; window.pendingSubmissions++; };
      });
      for (const autoSelect of [true, false]) {
        const target = `${base}/?host=pi`;
        await holdSubmission();
        await page.evaluate(auto => {
          const select = document.querySelector(".host-form select");
          if (auto) { select.value = "pi"; select.dispatchEvent(new Event("change", { bubbles: true })); }
          else document.querySelector(".host-form").requestSubmit();
        }, autoSelect);
        await page.waitForFunction(() => !!window.pendingForm);
        const feedback = await page.evaluate(() => ({
          status: document.querySelector("[data-ui-status]").textContent,
          hostDisabled: document.querySelector(".host-form select").disabled,
          enabledActions: document.querySelectorAll('.container-actions button:not([disabled])').length,
        }));
        assert(feedback.status.includes("Connecting to Raspberry Pi"), "slow host connection has no immediate feedback");
        assert.equal(feedback.hostDisabled, true, "host selection not locked while connecting");
        assert.equal(feedback.enabledActions, 0, "old-host actions remained enabled while connecting");
        await Promise.all([page.waitForEvent("load"), page.evaluate(() => window.nativeSubmit.call(window.pendingForm))]);
        await page.waitForURL(target);
        await page.waitForLoadState("load");
        assert.equal(await page.getByLabel("Docker host").inputValue(), "pi", "host selection missing from GET submission");
        assert.equal(await page.getByRole("button", { name: "Reconnect", exact: true }).isEnabled(), true);
      }
      const group = page.locator('details.stack-group[data-project="web"]');
      await group.locator("summary").click();
      const deleteForm = page.getByRole("form", { name: "Delete volume unused-volume", exact: true });
      await deleteForm.getByRole("button", { name: "Delete", exact: true }).click();
      assert.equal(await deleteForm.getByRole("checkbox").evaluate(input => input.validity.valueMissing), true);
      assert.equal(await page.getByLabel("Docker host").isEnabled(), true, "invalid confirmation locked page controls");
      const form = page.getByRole("form", { name: "Actions for web-running", exact: true });
      await form.getByLabel("Target").selectOption("stack");
      await holdSubmission();
      const requests = [];
      await page.route(`${base}/action`, async route => {
        requests.push(new URLSearchParams(route.request().postData()));
        await route.fulfill({ status: 303, headers: { location: `${base}/?host=pi&job=presentation-job` }, body: "" });
      });
      await page.evaluate(() => document.querySelector('.container-actions [value="upgrade"]').click());
      await page.waitForFunction(() => !!window.pendingForm);
      const feedback = await page.evaluate(() => {
        const form = document.querySelector('form[aria-label="Actions for web-running"]');
        const state = { status: document.querySelector("[data-ui-status]").textContent, busy: form.getAttribute("aria-busy"), disabled: form.querySelector('[value="upgrade"]').disabled };
        form.requestSubmit();
        return state;
      });
      assert(feedback.status.includes("Submitting Upgrade"), "slow submission has no immediate feedback");
      assert.equal(feedback.busy, "true");
      assert.equal(feedback.disabled, true);
      assert.equal(await page.evaluate(() => window.pendingSubmissions), 1, "duplicate submission reached native form handler");
      await Promise.all([page.waitForEvent("load"), page.evaluate(() => window.nativeSubmit.call(window.pendingForm))]);
      assert.equal(requests.length, 1, "duplicate submission reached server");
      const payload = requests[0];
      assert.equal(payload.get("host"), "pi");
      assert.equal(payload.get("id"), "running-id");
      assert.equal(payload.get("scope"), "stack");
      assert.deepEqual(payload.getAll("operation"), ["upgrade"], "disabled submitter lost or duplicated action");
      assert(payload.get("csrf_token"), "submission lost CSRF token");
      await page.waitForURL(`${base}/?host=pi&job=presentation-job`);
      assert.equal(await page.getByLabel("Operation output").isVisible(), true, "accepted operation did not open output");
      assert.deepEqual(errors, [], "submission/host feedback caused JavaScript errors");
      console.log(`PASS ${base}: immediate submission feedback, duplicate guard, automatic host selection and explicit reconnect`);
      await context.close();
    }
    for (const terminal of ["success", "failed"]) {
      const context = await browser.newContext({ viewport: { width: 900, height: 1000 } });
      const page = await context.newPage();
      const errors = [];
      page.on("pageerror", error => errors.push(error.message));
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      const url = `${base}/?host=pi&job=live-${terminal}`;
      let releaseInventory;
      let firstInventory = true;
      const inventoryGate = new Promise(resolve => { releaseInventory = resolve; });
      await page.route(`${base}/inventory?host=pi`, async route => {
        if (firstInventory) { firstInventory = false; await inventoryGate; }
        await route.continue();
      });
      if (terminal === "failed") await context.addInitScript(() => {
        Object.defineProperty(window, "localStorage", { get() { throw new DOMException("Storage disabled for test", "SecurityError"); } });
      });
      await page.goto(url);
      let navigations = 0;
      let inventoryRequests = 0;
      page.on("request", request => {
        if (request.url().includes("/inventory?")) {
          inventoryRequests++;
          assert.equal(new URL(request.url()).searchParams.get("host"), "pi", "inventory poll switched hosts");
        }
      });
      page.on("request", request => { if (request.isNavigationRequest() && request.frame() === page.mainFrame()) navigations++; });
      const output = page.getByLabel("Operation output");
      const atEnd = async () => {
        await page.waitForFunction(() => {
          const log = document.querySelector(".operation-output");
          return log.scrollHeight - log.clientHeight - log.scrollTop <= 2;
        }, null, { timeout: 1000 });
        return true;
      };
      assert.equal(await atEnd(), true, "initial log not scrolled to end");
      await output.evaluate(log => { log.scrollTop = 0; });
      await page.evaluate(() => { window.operationPageMarker = "unchanged"; });
      const web = page.locator('details.stack-group[data-project="web"]');
      await web.locator("summary").click();
      await page.waitForFunction(() => document.querySelector(".operation-output").textContent.endsWith("running output"));
      assert((await page.locator("[data-operation-message]").textContent()).includes("two hours"), "poll discarded build timeout notice");
      assert.equal(await atEnd(), true, "new output not scrolled to end");
      assert.equal(await page.evaluate(() => window.operationPageMarker), "unchanged", "poll reloaded page");
      const unchangedPoll = page.waitForResponse(response => response.url().includes("/operation?") && response.ok());
      await output.evaluate(log => { log.scrollTop = 0; });
      await unchangedPoll;
      await page.waitForFunction(() => {
        const log = document.querySelector(".operation-output");
        return log.scrollHeight - log.clientHeight - log.scrollTop <= 2;
      }, null, { timeout: 1000 });
      await page.getByRole("link", { name: "Close output", exact: true }).click();
      await page.waitForFunction(() => document.querySelector("[data-operation-message]").textContent.includes("Retrying automatically"));
      assert.equal(await page.locator("[data-ui-status]").isVisible(), true, "hidden output concealed connection feedback");
      assert((await page.locator("[data-ui-status]").textContent()).includes("Reconnecting to agent"));
      await page.getByRole("link", { name: "Show output", exact: true }).click();
      assert.equal(navigations, 0, "transient disconnect reloaded page");
      await page.waitForFunction(() => document.querySelector("[data-operation-status]").textContent === "updating");
      assert.equal(await atEnd(), true);
      assert.equal(await output.locator("script").count(), 0, "polled output executed as markup");
      assert((await output.textContent()).includes("<script>not executable</script>"));
      assert.equal(navigations, 0, "running operation navigated page");
      await page.getByRole("link", { name: "Close output", exact: true }).click();
      assert.equal(await output.isVisible(), false, "Close output did not hide panel");
      assert.equal(navigations, 0, "closing output navigated page");
      await page.getByRole("link", { name: "Show output", exact: true }).click();
      assert.equal(await output.isVisible(), true, "Show output did not restore panel");
      assert.equal(await atEnd(), true, "reopened output lost auto-scroll");
      assert.equal(navigations, 0, "reopening output navigated page");
      await page.getByRole("link", { name: "Close output", exact: true }).click();
      await page.waitForFunction(status => document.querySelector("[data-operation-status]").textContent === status, terminal);
      assert.equal(navigations, 0, "completion must update inventory without page navigation");
      assert.equal(await output.isVisible(), false, "completion reopened dismissed panel");
      assert((await page.locator("[data-ui-status]").textContent()).includes("Refreshing containers"), "slow inventory refresh has no visible feedback");
      assert.equal(await page.locator("[data-inventory]").getAttribute("aria-busy"), "true");
      releaseInventory();
      await page.waitForFunction(() => document.querySelector("[data-operation-message]").textContent.includes("inventory temporarily unavailable"));
      assert.equal(await page.locator('[data-build-component="agent"]').textContent(), "unavailable", "failed refresh retained stale agent commit");
      assert.equal(await page.getByRole("row").filter({ has: page.getByText("standalone", { exact: true }) }).count(), 0, "standalone group should remain collapsed");
      assert.equal(await page.locator('details.stack-group[data-project=""]').count(), 1, "inventory failure wiped existing rows");
      await page.waitForFunction(() => document.querySelector("[data-inventory]").textContent.includes("web-updated"));
      assert.equal(await page.locator('[data-build-component="agent"]').textContent(), "ccccccc", "inventory refresh did not update agent commit");
      assert.equal(inventoryRequests, 3, "completion must perform three bounded inventory checks");
      assert.equal(await web.evaluate(group => group.open), true, "inventory replacement lost accordion state");
      assert.equal(await web.locator('[name="id"][value="updated-id"]').count(), 1, "update kept old container ID");
      assert.equal(await web.getByRole("row").filter({ has: page.getByText("web-running", { exact: true }) }).locator(".badge-exited").count(), 1, "stop left stale running state");
      assert.equal(await page.locator('details.stack-group[data-project=""]').count(), 0, "deleted container remained listed");
      assert.equal(await page.locator('[name="id"][value="unused-volume"]').count(), 0, "deleted resource remained listed");
      await page.getByRole("link", { name: "Show output", exact: true }).click();
      assert.equal(await atEnd(), true, "completed log not scrolled to end");
      assert.equal(await page.evaluate(() => window.operationPageMarker), "unchanged", "inventory refresh replaced full page");
      await output.evaluate(log => { log.scrollTop = 0; });
      await page.reload();
      await page.waitForFunction(() => {
        const log = document.querySelector(".operation-output");
        return log.scrollHeight - log.clientHeight - log.scrollTop <= 2;
      });
      await page.setViewportSize({ width: 390, height: 700 });
      await page.waitForFunction(() => {
        const log = document.querySelector(".operation-output");
        return log.scrollHeight - log.clientHeight - log.scrollTop <= 2;
      }, null, { timeout: 1000 });
      assert.deepEqual(errors, [], "operation polling caused JavaScript errors");
      console.log(`PASS ${base}: async log/status/inventory, delayed Docker state, preserved accordions/output and reconnect (${terminal})`);
      await context.close();
    }
    {
      const context = await browser.newContext();
      const page = await context.newPage();
      await page.goto(`${base}/login`);
      await page.getByLabel("Admin password").fill("pw");
      await page.getByRole("button", { name: "Login", exact: true }).click();
      await page.waitForURL(`${base}/`);
      await page.goto(`${base}/?host=pi&job=live-expired`);
      assert((await page.locator("[data-ui-status]").textContent()).includes("Waiting for output"), "empty logs have no waiting feedback");
      await context.clearCookies();
      await page.waitForURL(`${base}/login`);
      assert.equal(await page.getByLabel("Admin password").count(), 1, "expired session did not require login");
      assert.equal(await page.getByLabel("Operation output").count(), 0, "expired session retained operation output");
      console.log(`PASS ${base}: operation polling returns expired sessions to login`);
      await context.close();
    }
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
