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

const uiStatus = document.querySelector("[data-ui-status]");
const showStatus = text => {
  uiStatus.textContent = text;
  uiStatus.hidden = !text;
};
const hostForm = document.querySelector(".host-form");
const setBuildCommit = (component, commit) => {
  commit ||= "unknown";
  const label = document.querySelector(`[data-build-component="${component}"]`);
  label.textContent = commit === "unavailable" ? commit : commit.slice(0, 7);
  label.title = commit;
  label.setAttribute("aria-label", (component === "ui" ? "UI" : "Agent") + " build commit " + commit);
};
hostForm.querySelector("select").addEventListener("change", () => hostForm.requestSubmit());
let submitting = false;
document.addEventListener("submit", event => {
  const form = event.target;
  if (!form.matches(".host-form, .container-actions, .cleanup-actions")) return;
  if (submitting) { event.preventDefault(); return; }
  event.preventDefault();
  submitting = true;
  const button = event.submitter || form.querySelector('button[type="submit"]');
  const hostChange = form === hostForm;
  if (hostChange) setBuildCommit("agent", "unavailable");
  const select = hostForm.querySelector("select");
  showStatus(hostChange
    ? "Connecting to " + select.selectedOptions[0].textContent + "…"
    : "Submitting " + button.textContent.trim() + "… Waiting for agent acceptance; do not resubmit.");
  // Disabled controls are omitted from native form submissions; retain the selected host/action.
  const name = hostChange ? select.name : button.name;
  if (name) {
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = hostChange ? select.value : button.value;
    form.append(input);
  }
  button.textContent = hostChange ? "Connecting…" : "Submitting…";
  form.setAttribute("aria-busy", "true");
  for (const control of document.querySelectorAll(".host-form select, .host-form button, .container-actions button, .cleanup-actions button")) {
    control.disabled = true;
  }
  // Paint feedback before native navigation can suspend the current document.
  window.requestAnimationFrame(() => window.setTimeout(() => form.submit(), 0));
});
window.addEventListener("pageshow", event => {
  if (event.persisted && submitting) window.location.reload();
});

if (document.querySelector("[data-maintenance]")) {
  const refreshMaintenance = async () => {
    if (submitting) return;
    const current = document.querySelector("[data-maintenance]");
    try {
      const response = await fetch(current.dataset.maintenanceUrl, { cache: "no-store" });
      if (response.redirected) { window.location.replace(response.url); return; }
      if (response.status === 409) { window.location.reload(); return; }
      if (!response.ok) throw new Error("Maintenance status unavailable");
      const next = new DOMParser().parseFromString(await response.text(), "text/html").querySelector("[data-maintenance]");
      if (!next) throw new Error("Invalid maintenance status");
      if (submitting) return;
      if (current.dataset.running === "true" && next.dataset.running !== "true") {
        window.location.reload();
        return;
      }
      if (current.dataset.running === "true" || next.dataset.running === "true") {
        const open = [...current.querySelectorAll("details")].map(item => item.open);
        [...next.querySelectorAll("details")].forEach((item, index) => item.open = open[index] || false);
        current.replaceWith(next);
      }
    } catch {
      showStatus("Reconnecting to maintenance coordinator. Do not resubmit updates.");
    }
    window.setTimeout(refreshMaintenance, 5000);
  };
  window.setTimeout(refreshMaintenance, 5000);
}

const operation = document.querySelector("[data-operation-url]");
if (operation) {
  const output = operation.querySelector(".operation-output");
  const badge = operation.querySelector("[data-operation-status]");
  const message = operation.querySelector("[data-operation-message]");
  const runningMessage = operation.dataset.status === "running"
    ? message.textContent
    : "Operation running. Output updates every three seconds; inventory refreshes when finished.";
  const setMessage = text => {
    message.textContent = text;
    if (!submitting) showStatus(text);
  };
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
    if (submitting) return;
    setMessage("Operation " + operation.dataset.status + ". Refreshing containers…");
    document.querySelector("[data-inventory]").setAttribute("aria-busy", "true");
    try {
      const current = document.querySelector("[data-inventory]");
      const response = await fetch(current.dataset.inventoryUrl, { cache: "no-store" });
      if (response.redirected) { window.location.replace(response.url); return; }
      if (!response.ok) throw new Error("Inventory unavailable");
      const next = new DOMParser().parseFromString(await response.text(), "text/html").querySelector("[data-inventory]");
      if (!next) throw new Error("Invalid inventory response");
      if (submitting) return;
      const states = new Map([...current.querySelectorAll("details.stack-group")].map(group => [group.dataset.project, group.open]));
      initGroups(next, states);
      setBuildCommit("agent", next.dataset.agentCommit);
      setBuildCommit("ui", next.dataset.uiCommit);
      current.replaceWith(next);
      setMessage("Operation " + operation.dataset.status + ". Inventory refreshed.");
    } catch {
      if (!submitting) setBuildCommit("agent", "unavailable");
      setMessage(remaining > 1
        ? "Operation finished; inventory temporarily unavailable. Retrying automatically."
        : "Operation finished; inventory could not be refreshed. Reload to check current host state.");
    } finally {
      document.querySelector("[data-inventory]").removeAttribute("aria-busy");
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
      setMessage(job.Status === "updating"
        ? "Management update running. Reconnecting automatically if the agent briefly disconnects."
        : runningMessage + (job.Output.trim() ? "" : " Waiting for output."));
    } catch {
      if (!submitting) setBuildCommit("agent", "unavailable");
      setMessage("Reconnecting to agent. Retrying automatically; do not resubmit.");
    }
    scrollToEnd();
    window.setTimeout(poll, 3000);
  };
  if (active(operation.dataset.status)) {
    setMessage((message.textContent.trim() || runningMessage) + (output.textContent.trim() ? "" : " Waiting for output."));
    window.setTimeout(poll, 3000);
  } else {
    setMessage("Operation " + operation.dataset.status + ". Checking container state…");
    window.setTimeout(() => refreshInventory(), 3000);
  }
}
