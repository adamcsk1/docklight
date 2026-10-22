for (const group of document.querySelectorAll("details.stack-group")) {
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
  group.addEventListener("toggle", () => {
    try {
      localStorage.setItem(key, group.open ? "open" : "closed");
    } catch {
      // Keep the current page usable when persistence is unavailable.
    }
  });
}

const operation = document.querySelector("[data-operation-url]");
if (operation) {
  const output = operation.querySelector(".operation-output");
  const badge = operation.querySelector("[data-operation-status]");
  const message = operation.querySelector("[data-operation-message]");
  const runningMessage = operation.dataset.status === "running"
    ? message.textContent
    : "Operation running. Output updates every three seconds; the page reloads when finished.";
  const scrollToEnd = () => window.requestAnimationFrame(() => {
    output.scrollTop = output.scrollHeight;
  });
  scrollToEnd();
  window.addEventListener("pageshow", scrollToEnd);
  new ResizeObserver(scrollToEnd).observe(output);
  const active = status => status === "running" || status === "updating";
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
      if (!active(job.Status)) {
        window.location.reload();
        return;
      }
      if (output.textContent !== job.Output) {
        output.textContent = job.Output;
      }
      badge.textContent = job.Status;
      badge.className = "badge badge-" + job.Status;
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
}
