const MAX_USAGE_TARGETS = 200;

document.addEventListener("DOMContentLoaded", () => {
  loadUsageMetrics();
});

async function loadUsageMetrics() {
  for (const section of document.querySelectorAll("[data-usage-url]")) {
    const url = usageURL(section);
    if (!url) continue;

    try {
      const response = await fetch(url, { headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`usage request failed: ${response.status}`);
      const metrics = await response.json();
      updateUsageSection(section, metrics || {});
    } catch {
      markUsageUnavailable(section);
    }
  }
}

function usageURL(section) {
  const url = section.dataset.usageUrl;
  if (!url) return "";

  const params = new URLSearchParams();
  let count = 0;
  for (const row of section.querySelectorAll("[data-usage-key]")) {
    const key = row.dataset.usageKey;
    const containers = (row.dataset.usageContainers || "").split(/\s+/).filter(Boolean);
    if (!key || containers.length === 0) continue;
    for (const container of containers) {
      if (count >= MAX_USAGE_TARGETS) break;
      params.append("target", `${key}|${container}`);
      count++;
    }
    if (count >= MAX_USAGE_TARGETS) break;
  }
  if ([...params].length === 0) return "";
  return `${url}?${params}`;
}

function updateUsageSection(section, metrics) {
  for (const row of section.querySelectorAll("[data-usage-key]")) {
    const key = row.dataset.usageKey;
    const target = row.querySelector("[data-usage-target]");
    if (!key || !(target instanceof HTMLElement)) continue;

    const metric = metrics[key] || {};
    setUsageText(target, metric.cpu || "-", metric.memory || "-");
  }
}

function markUsageUnavailable(section) {
  for (const target of section.querySelectorAll("[data-usage-target]")) {
    if (!(target instanceof HTMLElement)) continue;
    target.title = "usage unavailable";
  }
}

function setUsageText(target, cpu, memory) {
  if (target.tagName === "DIV") {
    target.replaceChildren(`CPU ${cpu}`, document.createElement("br"), `Memory ${memory}`);
    return;
  }
  target.textContent = `CPU ${cpu} / Memory ${memory}`;
}
