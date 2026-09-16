const test = require("node:test");
const assert = require("node:assert");
const query = require("../cmd/seekfs/ui_frontend/query.js");

test("normalizeLiveQuery blanks empty and incomplete input", () => {
  assert.equal(query.normalizeLiveQuery(""), "");
  assert.equal(query.normalizeLiveQuery("."), "");
  assert.equal(query.normalizeLiveQuery("ext:"), "");
  assert.equal(query.normalizeLiveQuery("main.go ."), "");
  assert.equal(query.normalizeLiveQuery("?"), "?");
  assert.equal(query.normalizeLiveQuery("main.go"), "main.go");
  assert.equal(query.normalizeLiveQuery("dir:cmd main"), "dir:cmd main");
});

test("hasIncompleteTrailingToken flags mid-token input", () => {
  assert.equal(query.hasIncompleteTrailingToken("main.go"), false);
  assert.equal(query.hasIncompleteTrailingToken("dir:src"), false);
  assert.equal(query.hasIncompleteTrailingToken("dir:"), true);
  assert.equal(query.hasIncompleteTrailingToken("a b ."), true);
  assert.equal(query.hasIncompleteTrailingToken(""), false);
});
