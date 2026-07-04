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
    removeManagedRow(removeManagedFolder, "folder");
    return;
  }

  if (target.closest("[data-managed-file-add]")) {
    addManagedRow(target, "file");
    return;
  }

  const removeManagedFile = target.closest("[data-managed-file-remove]");
  if (removeManagedFile) {
    removeManagedRow(removeManagedFile, "file");
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

function removeManagedRow(target, type) {
  const row = target.closest(`.managed-${type}-row`);
  if (!(row instanceof HTMLElement)) return;

  if (row.dataset.pendingDelete === "true") {
    undoPendingManagedDelete(row, type, target);
    return;
  }

  const deleteInput = row.querySelector(`[data-delete-managed-${type}]`);
  const existingPath = row.querySelector(`input[name="existing_managed_${type}_path"]`);
  if (deleteInput instanceof HTMLInputElement && deleteInput.checked && existingPath instanceof HTMLInputElement) {
    markPendingManagedDelete(row, type, existingPath.value, target);
    return;
  }

  row.remove();
}

function markPendingManagedDelete(row, type, path, button) {
  const hidden = document.createElement("input");
  hidden.type = "hidden";
  hidden.name = `delete_managed_${type}_path`;
  hidden.value = path;
  hidden.dataset.pendingManagedDelete = "true";
  row.append(hidden);

  for (const control of row.querySelectorAll("input, textarea")) {
    if (control !== hidden && (control instanceof HTMLInputElement || control instanceof HTMLTextAreaElement)) {
      control.disabled = true;
    }
  }

  const note = document.createElement("p");
  note.className = "form-hint";
  note.dataset.pendingManagedDeleteNote = "true";
  note.textContent = `Will stop managing and delete ${path} from disk when saved.`;
  row.append(note);
  row.dataset.pendingDelete = "true";
  button.textContent = `Keep managing ${type}`;
}

function undoPendingManagedDelete(row, type, button) {
  row.querySelector("[data-pending-managed-delete]")?.remove();
  row.querySelector("[data-pending-managed-delete-note]")?.remove();
  for (const control of row.querySelectorAll("input, textarea")) {
    if (control instanceof HTMLInputElement || control instanceof HTMLTextAreaElement) {
      control.disabled = false;
    }
  }
  const deleteInput = row.querySelector(`[data-delete-managed-${type}]`);
  if (deleteInput instanceof HTMLInputElement) {
    deleteInput.checked = false;
  }
  delete row.dataset.pendingDelete;
  button.textContent = `Stop managing ${type}`;
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

function confirmActionSubmit(form, submitter) {
  const managedDeleteText = managedDiskDeleteText(form);
  const action = submitter?.dataset.confirmAction || form.dataset.confirmAction;
  if (action) {
    const irreversibleText = managedDeleteText ? "\n\nThis cannot be undone." : "";
    return window.confirm(action + managedDeleteText + irreversibleText);
  }

  const target = submitter?.dataset.confirmDelete || form.dataset.confirmDelete;
  if (!target) {
    if (!managedDeleteText) return true;
    return window.confirm(`Delete managed paths from disk?${managedDeleteText}\n\nThis cannot be undone.`);
  }

  const selected = selectedForForm(form).length;
  const countText = selected > 0 ? `\nSelected items: ${selected}` : "";
  const force = form.querySelector('input[name="force"]:checked');
  const forceText = force ? "\n\nForce is enabled. Docker may remove running or in-use resources if allowed by the server." : "";
  const message = `Delete ${target}?${countText}${forceText}${managedDeleteText}\n\nThis cannot be undone.`;

  return window.confirm(message);
}

function managedDiskDeleteText(form) {
  const deletes = form.querySelectorAll('input[name="delete_managed_file_path"]:checked:not(:disabled), input[name="delete_managed_file_path"][type="hidden"]:not(:disabled), input[name="delete_managed_folder_path"]:checked:not(:disabled), input[name="delete_managed_folder_path"][type="hidden"]:not(:disabled)');
  if (deletes.length === 0) return "";
  const paths = [...deletes].map((input) => input.value).filter(Boolean);
  return `\n\nManaged paths marked for disk deletion:\n- ${paths.join("\n- ")}`;
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
