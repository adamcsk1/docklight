document.addEventListener("DOMContentLoaded", () => {
  const panel = document.querySelector("[data-container-log]");
  if (!(panel instanceof HTMLElement)) return;

  const refresh = panel.querySelector("[data-log-refresh]");
  const copy = panel.querySelector("[data-log-copy]");
  const autoRefresh = panel.querySelector("[data-log-auto-refresh]");
  const output = panel.querySelector("[data-log-output]");
  const status = panel.querySelector("[data-log-status]");
  const url = panel.dataset.logUrl;
  if (!(refresh instanceof HTMLButtonElement) || !(copy instanceof HTMLButtonElement) || !(autoRefresh instanceof HTMLInputElement) || !(output instanceof HTMLElement) || !(status instanceof HTMLElement) || !url) return;

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

  const copyWithFallback = (text) => {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.setAttribute("readonly", "");
    textarea.style.position = "fixed";
    textarea.style.top = "0";
    textarea.style.left = "0";
    textarea.style.opacity = "0";
    document.body.appendChild(textarea);
    textarea.select();
    try {
      return document.execCommand("copy");
    } catch {
      return false;
    } finally {
      textarea.remove();
    }
  };

  const copyLogs = async () => {
    const text = output.textContent || "";
    copy.disabled = true;
    try {
      if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
        throw new Error("clipboard api unavailable");
      }
      await navigator.clipboard.writeText(text);
      status.textContent = "Copied log output to clipboard.";
    } catch {
      if (copyWithFallback(text)) {
        status.textContent = "Copied log output to clipboard.";
      } else {
        status.textContent = "Copy failed.";
      }
    } finally {
      copy.disabled = false;
    }
  };

  refresh.addEventListener("click", refreshLogs);
  copy.addEventListener("click", copyLogs);
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
