document.addEventListener("DOMContentLoaded", () => {
  const panel = document.querySelector("[data-container-log]");
  if (!(panel instanceof HTMLElement)) return;

  const refresh = panel.querySelector("[data-log-refresh]");
  const autoRefresh = panel.querySelector("[data-log-auto-refresh]");
  const output = panel.querySelector("[data-log-output]");
  const status = panel.querySelector("[data-log-status]");
  const url = panel.dataset.logUrl;
  if (!(refresh instanceof HTMLButtonElement) || !(autoRefresh instanceof HTMLInputElement) || !(output instanceof HTMLElement) || !(status instanceof HTMLElement) || !url) return;

  let timer = 0;
  const refreshLogs = async () => {
    refresh.disabled = true;
    status.textContent = "Refreshing...";
    try {
      const response = await fetch(url, { headers: { Accept: "text/plain" } });
      if (!response.ok) throw new Error(`log request failed: ${response.status}`);
      output.textContent = await response.text();
      status.textContent = `Last refreshed ${new Date().toLocaleTimeString()}. Showing last 300 lines.`;
    } catch {
      status.textContent = "Refresh failed. Existing logs are still shown.";
    } finally {
      refresh.disabled = false;
    }
  };

  refresh.addEventListener("click", refreshLogs);
  autoRefresh.addEventListener("change", () => {
    if (timer) {
      window.clearInterval(timer);
      timer = 0;
    }
    if (autoRefresh.checked) {
      refreshLogs();
      timer = window.setInterval(refreshLogs, 5000);
    }
  });
  window.addEventListener("beforeunload", () => {
    if (timer) window.clearInterval(timer);
  });
});
