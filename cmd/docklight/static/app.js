function initGroups(root, states = new Map()) {
  for (const group of root.querySelectorAll("details.stack-group")) {
    const key = "docklight:accordion:" + JSON.stringify([
      group.dataset.base,
      group.dataset.host,
      group.dataset.project,
    ]);
    try {
      group.open = localStorage.getItem(key) === "open";
    } catch {
      // Browser storage can be disabled; native accordions still work.
    }
    if (states.has(group.dataset.project)) group.open = states.get(group.dataset.project);
    group.addEventListener("toggle", () => {
      try {
        localStorage.setItem(key, group.open ? "open" : "closed");
      } catch {
        // Keep the current page usable when persistence is unavailable.
      }
    });
  }
}
initGroups(document);

const operation = document.querySelector("[data-operation-url]");
if (operation) {
  const output = operation.querySelector(".operation-output");
  const badge = operation.querySelector("[data-operation-status]");
  const message = operation.querySelector("[data-operation-message]");
  const runningMessage = operation.dataset.status === "running"
    ? message.textContent
    : "Operation running. Output updates every three seconds; inventory refreshes when finished.";
  const scrollToEnd = () => window.requestAnimationFrame(() => {
    output.scrollTop = output.scrollHeight;
  });
  scrollToEnd();
  window.addEventListener("pageshow", scrollToEnd);
  new ResizeObserver(scrollToEnd).observe(output);
  const reopen = document.querySelector("[data-operation-reopen]");
  for (const link of document.querySelectorAll("[data-operation-toggle]")) {
    link.addEventListener("click", event => {
      if (event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      operation.hidden = link.dataset.operationToggle === "hide";
      reopen.hidden = !operation.hidden;
      window.history.replaceState(null, "", link.href);
      (operation.hidden ? reopen.querySelector("a") : operation.querySelector("[data-operation-toggle]")).focus({ preventScroll: true });
      if (!operation.hidden) scrollToEnd();
    });
  }
  const active = status => status === "running" || status === "updating";
  const refreshInventory = async (remaining = 3) => {
    try {
      const current = document.querySelector("[data-inventory]");
      const response = await fetch(current.dataset.inventoryUrl, { cache: "no-store" });
      if (response.redirected) { window.location.replace(response.url); return; }
      if (!response.ok) throw new Error("Inventory unavailable");
      const next = new DOMParser().parseFromString(await response.text(), "text/html").querySelector("[data-inventory]");
      if (!next) throw new Error("Invalid inventory response");
      const states = new Map([...current.querySelectorAll("details.stack-group")].map(group => [group.dataset.project, group.open]));
      initGroups(next, states);
      current.replaceWith(next);
      message.textContent = "Operation " + operation.dataset.status + ". Inventory refreshed.";
    } catch {
      message.textContent = remaining > 1
        ? "Operation finished; inventory temporarily unavailable. Retrying automatically."
        : "Operation finished; inventory could not be refreshed. Reload to check current host state.";
    }
    if (remaining > 1) window.setTimeout(() => refreshInventory(remaining - 1), 3000);
  };
  const poll = async () => {
    try {
      const response = await fetch(operation.dataset.operationUrl, { cache: "no-store" });
      if (response.redirected) {
        window.location.replace(response.url);
        return;
      }
      if (!response.ok) throw new Error("Operation unavailable");
      const job = await response.json();
      if (!["running", "updating", "success", "failed"].includes(job.Status) || typeof job.Output !== "string") {
        throw new Error("Invalid operation response");
      }
      if (output.textContent !== job.Output) {
        output.textContent = job.Output;
      }
      badge.textContent = job.Status;
      badge.className = "badge badge-" + job.Status;
      operation.dataset.status = job.Status;
      if (!active(job.Status)) {
        scrollToEnd();
        await refreshInventory();
        return;
      }
      message.textContent = job.Status === "updating"
        ? "Management update running. Reconnecting automatically if the agent briefly disconnects."
        : runningMessage;
    } catch {
      message.textContent = "Operation temporarily unavailable. Retrying automatically; do not resubmit.";
    }
    scrollToEnd();
    window.setTimeout(poll, 3000);
  };
  if (active(operation.dataset.status)) window.setTimeout(poll, 3000);
  else window.setTimeout(() => refreshInventory(), 3000);
}
