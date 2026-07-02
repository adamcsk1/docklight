document.addEventListener("click", (event) => {
  const target = event.target;
  if (!(target instanceof Element)) return;

  const toggle = target.closest(".menu-toggle");
  if (target.closest("[data-managed-folder-add]")) {
    addManagedRow(target, "folder");
    return;
  }

  const removeManagedFolder = target.closest("[data-managed-folder-remove]");
  if (removeManagedFolder) {
    removeManagedFolder.closest(".managed-folder-row")?.remove();
    return;
  }

  if (target.closest("[data-managed-file-add]")) {
    addManagedRow(target, "file");
    return;
  }

  const removeManagedFile = target.closest("[data-managed-file-remove]");
  if (removeManagedFile) {
    removeManagedFile.closest(".managed-file-row")?.remove();
    return;
  }

  if (!(toggle instanceof HTMLButtonElement)) return;

  const menuID = toggle.getAttribute("aria-controls");
  const menu = menuID ? document.getElementById(menuID) : null;
  if (!menu) return;

  const expanded = toggle.getAttribute("aria-expanded") === "true";
  toggle.setAttribute("aria-expanded", String(!expanded));
  menu.classList.toggle("is-open", !expanded);
});

function addManagedRow(target, type) {
  const form = target.closest("form");
  const list = form?.querySelector(`[data-managed-${type}-list]`);
  const template = form?.querySelector(`template[data-managed-${type}-template]`);
  if (!(list instanceof HTMLElement) || !(template instanceof HTMLTemplateElement)) return;

  list.append(template.content.cloneNode(true));
}

document.addEventListener("change", (event) => {
  const target = event.target;
  if (!(target instanceof HTMLInputElement)) return;

  const form = target.closest(".resource-form");
  if (!form) return;

  if (target.matches("[data-select-all]")) {
    for (const checkbox of form.querySelectorAll('input[name="ids"]:not(:disabled)')) {
      checkbox.checked = target.checked;
    }
    return;
  }

  if (target.matches('input[name="ids"]')) {
    const selectAll = form.querySelector("[data-select-all]");
    if (!(selectAll instanceof HTMLInputElement)) return;

    const checkboxes = [...form.querySelectorAll('input[name="ids"]:not(:disabled)')];
    const checked = checkboxes.filter((checkbox) => checkbox.checked).length;
    selectAll.checked = checkboxes.length > 0 && checked === checkboxes.length;
    selectAll.indeterminate = checked > 0 && checked < checkboxes.length;
  }
});

document.addEventListener("submit", (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement)) return;

  const submitter = event.submitter instanceof HTMLElement ? event.submitter : null;
  if (!confirmActionSubmit(form, submitter)) {
    event.preventDefault();
    return;
  }

  if (form.dataset.busy === "true") {
    event.preventDefault();
    return;
  }

  form.dataset.busy = "true";
  form.setAttribute("aria-busy", "true");

  if (submitter instanceof HTMLButtonElement || submitter instanceof HTMLInputElement) {
    if (submitter.name && !submitter.disabled) {
      const mirror = document.createElement("input");
      mirror.type = "hidden";
      mirror.name = submitter.name;
      mirror.value = submitter.value;
      mirror.dataset.submitterMirror = "true";
      form.append(mirror);
    }
    setLoadingSubmitter(submitter);
  }

  for (const control of form.querySelectorAll('button[type="submit"], input[type="submit"], button:not([type])')) {
    if (control instanceof HTMLButtonElement || control instanceof HTMLInputElement) {
      control.disabled = true;
    }
  }
});

document.addEventListener("DOMContentLoaded", () => {
  for (const panel of document.querySelectorAll("[data-operation-status-url]")) {
    if (panel instanceof HTMLElement) handleOperationPanel(panel);
  }
});

function handleOperationPanel(panel) {
  if (panel.dataset.operationPending === "true") {
    pollOperation(panel);
    return;
  }
  if (panel.dataset.operationStatus === "success") {
    refreshOperationPage();
  }
}

function refreshOperationPage() {
  window.setTimeout(() => {
    const url = new URL(window.location.href);
    url.searchParams.delete("operation");
    window.location.assign(url.toString());
  }, 1500);
}

function pollOperation(panel) {
  const url = panel.dataset.operationStatusUrl;
  if (!url || panel.dataset.operationPending !== "true") return;

  const badge = panel.querySelector("[data-operation-badge]");
  const message = panel.querySelector("[data-operation-message]");
  const output = panel.querySelector("[data-operation-output]");

  const poll = async () => {
    try {
      const response = await fetch(url, { headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`status ${response.status}`);
      const activity = await response.json();
      if (badge instanceof HTMLElement) {
        badge.textContent = activity.Status || activity.status || "unknown";
        badge.className = `badge badge-${badge.textContent}`;
      }
      if (output instanceof HTMLElement) output.textContent = activity.Output || activity.output || "";
      const status = activity.Status || activity.status || "";
      if (status === "running" || status === "started") {
        window.setTimeout(poll, 2500);
        return;
      }
      panel.dataset.operationPending = "false";
      if (message instanceof HTMLElement) message.textContent = status === "success" ? "Operation completed. Refreshing stack state..." : "Operation failed. Output is shown below.";
      if (status === "success") refreshOperationPage();
    } catch (error) {
      if (message instanceof HTMLElement) message.textContent = `Could not poll operation status: ${error}`;
      window.setTimeout(poll, 5000);
    }
  };

  window.setTimeout(poll, 1000);
}

function confirmActionSubmit(form, submitter) {
  const action = submitter?.dataset.confirmAction || form.dataset.confirmAction;
  if (action) {
    return window.confirm(action);
  }

  const target = submitter?.dataset.confirmDelete || form.dataset.confirmDelete;
  if (!target) return true;

  const selected = selectedForForm(form).length;
  const countText = selected > 0 ? `\nSelected items: ${selected}` : "";
  const force = form.querySelector('input[name="force"]:checked');
  const forceText = force ? "\n\nForce is enabled. Docker may remove running or in-use resources if allowed by the server." : "";
  const message = `Delete ${target}?${countText}${forceText}\n\nThis cannot be undone.`;

  return window.confirm(message);
}

function selectedForForm(form) {
  const selected = [...form.querySelectorAll('input[name="ids"]:checked:not(:disabled)')];
  if (!form.id) return selected;

  const escapedID = window.CSS?.escape ? CSS.escape(form.id) : form.id.replace(/"/g, '\\"');
  selected.push(...document.querySelectorAll(`input[name="ids"][form="${escapedID}"]:checked:not(:disabled)`));
  return selected;
}

function setLoadingSubmitter(submitter) {
  const loadingText = submitter.dataset.loadingText || "Working...";
  submitter.classList.add("is-loading");

  if (submitter instanceof HTMLButtonElement) {
    submitter.dataset.originalText = submitter.textContent || "";
    submitter.textContent = loadingText;
    return;
  }

  submitter.dataset.originalValue = submitter.value;
  submitter.value = loadingText;
}
