// Pure formatting and predicate helpers shared by the UI frontend and its node
// tests.  Loaded as a classic script in index.html (exposes window.SeekfsUtil)
// and require()-able from node so they can be unit tested without a DOM.
(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.SeekfsUtil = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  function debounce(fn, delay) {
    let handle = 0;
    return (...args) => {
      clearTimeout(handle);
      handle = setTimeout(() => fn(...args), delay);
    };
  }

  function formatSize(row) {
    if (!row.exists) return "";
    if (row.size === undefined || row.size === null) return "";
    const size = Number(row.size || 0);
    if (size < 1024) return `${size.toLocaleString()} B`;
    if (size < 1024 * 1024) return `${Math.ceil(size / 1024).toLocaleString()} KB`;
    if (size < 1024 * 1024 * 1024) return `${Math.ceil(size / 1024 / 1024).toLocaleString()} MB`;
    if (size < 1024 * 1024 * 1024 * 1024) return `${Math.ceil(size / 1024 / 1024 / 1024).toLocaleString()} GB`;
    return `${Math.ceil(size / 1024 / 1024 / 1024 / 1024).toLocaleString()} TB`;
  }

  function formatDate(value) {
    if (!value) return "";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "";
    return date.toLocaleString();
  }

  // escapeHtml neutralizes the characters that could break out of the row
  // markup built from indexed file names.
  function escapeHtml(value) {
    return String(value)
      .replaceAll("&", "&amp;")
      .replaceAll("<", "&lt;")
      .replaceAll(">", "&gt;")
      .replaceAll('"', "&quot;");
  }

  function sortSupported(column) {
    return column === "size" || column === "modified" || column === "extension" || column === "type" || column === "path";
  }

  return { debounce, formatSize, formatDate, escapeHtml, sortSupported };
});
