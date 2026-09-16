const test = require("node:test");
const assert = require("node:assert");
const util = require("../cmd/seekfs/ui_frontend/util.js");

test("formatSize scales and blanks missing sizes", () => {
  assert.equal(util.formatSize({ exists: false, size: 100 }), "");
  assert.equal(util.formatSize({ exists: true }), "");
  assert.equal(util.formatSize({ exists: true, size: null }), "");
  assert.equal(util.formatSize({ exists: true, size: 0 }), "0 B");
  assert.equal(util.formatSize({ exists: true, size: 512 }), "512 B");
  assert.equal(util.formatSize({ exists: true, size: 1536 }), "2 KB");
  assert.equal(util.formatSize({ exists: true, size: 1048576 }), "1 MB");
  assert.equal(util.formatSize({ exists: true, size: 1073741824 }), "1 GB");
  assert.equal(util.formatSize({ exists: true, size: 1099511627776 }), "1 TB");
});

test("formatDate blanks empty and invalid values", () => {
  assert.equal(util.formatDate(""), "");
  assert.equal(util.formatDate("not-a-date"), "");
  assert.notEqual(util.formatDate("2026-01-02T03:04:05Z"), "");
});

test("escapeHtml neutralizes markup characters", () => {
  assert.equal(util.escapeHtml('<a href="x">&'), "&lt;a href=&quot;x&quot;&gt;&amp;");
  assert.equal(util.escapeHtml("plain"), "plain");
});

test("sortSupported only accepts service-sortable columns", () => {
  for (const column of ["size", "modified", "extension", "type", "path"]) {
    assert.equal(util.sortSupported(column), true, column);
  }
  for (const column of ["name", "", "nonsense"]) {
    assert.equal(util.sortSupported(column), false, column);
  }
});

test("debounce collapses rapid calls into one", async () => {
  let calls = 0;
  const fn = util.debounce(() => {
    calls += 1;
  }, 20);
  fn();
  fn();
  fn();
  assert.equal(calls, 0);
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal(calls, 1);
});
