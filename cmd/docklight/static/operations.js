(function () {
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
        setOutputValue(output, activity.Output || activity.output || "");
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

  function setOutputValue(output, text) {
    if (output instanceof HTMLTextAreaElement) {
      output.value = text;
      return;
    }
    if (output instanceof HTMLElement) output.textContent = text;
  }
})();
