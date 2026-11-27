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
        for (const layoutWidth of width === 900 ? [673, 672, width] : [width]) {
          await page.setViewportSize({ width: layoutWidth, height: 900 });
          const headings = await page.locator(".stack-summary").evaluateAll(summaries => summaries.map(summary => {
            const heading = summary.querySelector(".stack-heading");
            const name = heading.querySelector(".stack-name");
            return {
              project: summary.parentElement.dataset.project,
              display: getComputedStyle(summary).display,
              gap: parseFloat(getComputedStyle(heading).columnGap),
              heading: heading.getBoundingClientRect().toJSON(),
              name: name.getBoundingClientRect().toJSON(),
              badges: [...heading.querySelectorAll(".badge")].map(badge => ({ ...badge.getBoundingClientRect().toJSON(), text: badge.textContent.trim() })),
            };
          }));
          assert.equal(headings.length, 6, "layout fixture must include varied stack names and status combinations");
          for (const { project, display, gap, heading, name, badges } of headings) {
            const label = `${project || "standalone"} at ${layoutWidth}px`;
            assert.equal(display, "list-item", `native disclosure marker lost: ${label}`);
            assert(name.left >= heading.left - 1 && name.right <= heading.right + 1, `name outside summary: ${label}`);
            for (const badge of badges) {
              assert(badge.left >= heading.left - 1 && badge.right <= heading.right + 1, `badge outside summary: ${label}`);
              assert(badge.top >= heading.top - 1 && badge.bottom <= heading.bottom + 1, `badge outside summary height: ${label}`);
            }
            if (badges[0].top < name.bottom - 1) {
              assert(badges[0].left >= name.right + gap - 1, `badges overlap stack name: ${label}`);
            }
            if (layoutWidth > 672) {
              assert(Math.abs(badges.at(-1).right - heading.right) <= 1, `status cluster not right-aligned: ${label}`);
              for (let i = 1; i < badges.length; i++) {
                assert(Math.abs(badges[i].top - badges[i - 1].top) <= 1, `desktop badges split across rows: ${label}`);
                assert(Math.abs(badges[i].left - badges[i - 1].right - gap) <= 1, `badges scattered instead of grouped: ${label}`);
              }
            } else {
              assert(badges[0].top >= name.bottom, `mobile badges must sit below stack name: ${label}`);
              assert(Math.abs(badges[0].left - name.left) <= 1, `mobile status cluster not left-aligned: ${label}`);
            }
          }
          assert.equal(headings.find(item => item.project === "a").badges.length, 1);
          assert.equal(headings.find(item => item.project === "medium-stack-name").badges.length, 2);
          assert.deepEqual(headings.find(item => item.project === "stack-with-mixed-update-states").badges.map(badge => badge.text), ["1 / 2 running", "↑ 1 service with updates", "1 update checks unknown/stale"]);
          assert.deepEqual(headings.find(item => item.project === "").badges.map(badge => badge.text), ["1 / 1 running"]);
        }
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
    console.log("PASS grouped stack status alignment, long names, mobile wrapping, image-update badges, keyboard details and refresh preservation with/without JavaScript");
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
