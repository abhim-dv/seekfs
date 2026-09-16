// Pure query helpers shared by the UI frontend and its node tests.
//
// Loaded as a classic script in index.html (exposes window.SeekfsQuery) and
// require()-able from node so the helpers can be unit tested without a DOM.
(function (root, factory) {
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.SeekfsQuery = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  // hasIncompleteTrailingToken reports whether the user is mid-token, e.g. a
  // trailing "." or a bare "ext:" filter with no value yet.
  function hasIncompleteTrailingToken(query) {
    const trimmed = query.trim();
    if (!trimmed) return false;
    const fields = trimmed.split(/\s+/);
    const last = fields[fields.length - 1] || "";
    return last === "." || /:$/i.test(last);
  }

  // normalizeLiveQuery blanks partial input so live search never queries the
  // service for an unfinished token.
  function normalizeLiveQuery(query) {
    if (!query) return "";
    if (/^[^\w*?]{1}$/.test(query)) return "";
    if (hasIncompleteTrailingToken(query)) return "";
    return query;
  }

  return { hasIncompleteTrailingToken, normalizeLiveQuery };
});
