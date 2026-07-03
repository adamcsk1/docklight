(function () {
  function fallbackCopy(text) {
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
  }

  async function copyText(text) {
    try {
      if (!navigator.clipboard || typeof navigator.clipboard.writeText !== "function") {
        throw new Error("clipboard api unavailable");
      }
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      return fallbackCopy(text);
    }
  }

  function elementText(element) {
    if (element instanceof HTMLTextAreaElement || element instanceof HTMLInputElement) return element.value;
    if (element instanceof HTMLElement) return element.textContent || "";
    return "";
  }

  async function copyOutput(button) {
    const block = button.closest(".output-block") || button.closest(".panel") || button.closest(".activity-item");
    const output = block?.querySelector("[data-output-copy-source]");
    const status = block?.querySelector("[data-output-copy-status]");
    button.disabled = true;
    const copied = await copyText(elementText(output));
    if (status instanceof HTMLElement) status.textContent = copied ? "Copied output to clipboard." : "Copy failed.";
    button.disabled = false;
  }

  document.addEventListener("click", (event) => {
    const target = event.target;
    if (!(target instanceof Element)) return;

    const button = target.closest("[data-output-copy]");
    if (button instanceof HTMLButtonElement) copyOutput(button);
  });

  window.DocklightClipboard = { copyText, elementText };
})();
