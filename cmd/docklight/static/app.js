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
