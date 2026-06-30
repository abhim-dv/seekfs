const state = {
  seq: 0,
  rows: [],
  selected: -1,
  lastQuery: "",
};

const els = {
  query: document.getElementById("query"),
  results: document.getElementById("results"),
  empty: document.getElementById("empty"),
  summary: document.getElementById("summary"),
  selected: document.getElementById("selected"),
  menu: document.getElementById("menu"),
};

function api() {
  return window.go && window.go.main && window.go.main.UIApp;
}

async function call(method, ...args) {
  const backend = await waitForBackend();
  if (!backend || !backend[method]) throw new Error("seekfs UI backend is not ready");
  return backend[method](...args);
}

function waitForBackend() {
  const ready = api();
  if (ready) return Promise.resolve(ready);
  return new Promise((resolve) => {
    const started = Date.now();
    const timer = setInterval(() => {
      const backend = api();
      if (backend || Date.now() - started > 2500) {
        clearInterval(timer);
        resolve(backend);
      }
    }, 25);
  });
}

function waitForRuntime() {
  if (window.runtime && window.runtime.EventsOn) return Promise.resolve(window.runtime);
  return new Promise((resolve) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if ((window.runtime && window.runtime.EventsOn) || Date.now() - started > 2500) {
        clearInterval(timer);
        resolve(window.runtime);
      }
    }, 25);
  });
}

function debounce(fn, delay) {
  let handle = 0;
  return (...args) => {
    clearTimeout(handle);
    handle = setTimeout(() => fn(...args), delay);
  };
}

function selectedRows() {
  if (state.selected < 0 || state.selected >= state.rows.length) return [];
  return [state.rows[state.selected]];
}

function selectedPaths() {
  return selectedRows().map((row) => row.path);
}

function setSelected(index) {
  state.selected = index;
  [...els.results.querySelectorAll("tr")].forEach((tr, i) => {
    tr.classList.toggle("selected", i === index);
  });
  const row = selectedRows()[0];
  els.selected.textContent = row ? row.path : "";
}

function formatSize(row) {
  if (row.is_dir) return "";
  if (!row.exists) return "";
  if (row.size === undefined || row.size === null) return "";
  const size = Number(row.size || 0);
  if (size < 1024) return `${size.toLocaleString()} B`;
  if (size < 1024 * 1024) return `${Math.ceil(size / 1024).toLocaleString()} KB`;
  return `${Math.ceil(size / 1024 / 1024).toLocaleString()} MB`;
}

function formatDate(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString();
}

function escapeHtml(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function fileGlyph(row) {
  return row.is_dir ? "▸" : "◻";
}

function renderRows(rows) {
  state.rows = rows;
  state.selected = rows.length ? 0 : -1;
  els.results.textContent = "";
  const frag = document.createDocumentFragment();
  rows.forEach((row, index) => {
    const tr = document.createElement("tr");
    tr.title = row.path;
    tr.dataset.index = String(index);
    tr.className = index === state.selected ? "selected" : "";
    tr.innerHTML = `
      <td>${escapeHtml(row.name || "")}</td>
      <td class="path-cell">${escapeHtml(row.dir || row.path || "")}</td>
      <td class="size-cell">${escapeHtml(formatSize(row))}</td>
      <td class="date-cell">${escapeHtml(formatDate(row.modified))}</td>
    `;
    tr.addEventListener("click", () => setSelected(index));
    tr.addEventListener("dblclick", () => openSelected());
    tr.addEventListener("contextmenu", (event) => {
      event.preventDefault();
      setSelected(index);
      showMenu(event.clientX, event.clientY);
    });
    frag.appendChild(tr);
  });
  els.results.appendChild(frag);
  els.empty.style.display = rows.length ? "none" : "block";
  const row = selectedRows()[0];
  els.selected.textContent = row ? row.path : "";
}

async function refreshStatus() {
  try {
    const status = await call("Status");
    if (!status.ok) {
      els.summary.textContent = status.message || "Service unavailable";
      return;
    }
    const loading = status.loading ? " loading" : "";
    if (!state.lastQuery) {
      els.summary.textContent = `${status.entries.toLocaleString()} items${loading}`;
    }
  } catch (err) {
    els.summary.textContent = err.message;
  }
}

async function searchNow() {
  const rawQuery = els.query.value.trim();
  const query = normalizeLiveQuery(rawQuery);
  state.lastQuery = rawQuery;
  const seq = ++state.seq;
  hideMenu();
  if (!rawQuery) {
    renderRows([]);
    els.summary.textContent = "Ready";
    els.empty.textContent = "Type to search";
    return;
  }
  if (!query) {
    renderRows([]);
    els.summary.textContent = "Keep typing";
    els.empty.textContent = "Type to search";
    return;
  }
  els.summary.textContent = "Searching...";
  try {
    await call("SearchAsync", {
      query,
      limit: 200,
    }, seq);
  } catch (err) {
    if (seq !== state.seq) return;
    renderRows([]);
    els.empty.textContent = err.message;
    els.summary.textContent = err.message;
  }
}

function normalizeLiveQuery(query) {
  if (!query) return "";
  if (/^[^\w*?]{1}$/.test(query)) return "";
  if (hasIncompleteTrailingToken(query)) return "";
  return query;
}

function hasIncompleteTrailingToken(query) {
  const trimmed = query.trim();
  if (!trimmed) return false;
  const fields = trimmed.split(/\s+/);
  const last = fields[fields.length - 1] || "";
  return last === "." || /:$/i.test(last);
}

function handleSearchResponse(response) {
  if (!response || response.seq !== state.seq) return;
  if ((response.message || "").toLowerCase() === "query superseded") return;
  if (!response.ok) {
    renderRows([]);
    els.empty.textContent = response.message || "Search failed";
    els.summary.textContent = response.message || "Search failed";
    return;
  }
  renderRows(response.results || []);
  const count = response.count || (response.results || []).length;
  els.summary.textContent = `${count.toLocaleString()} items  |  ${response.elapsed_ms} ms`;
  els.empty.textContent = "No matches";
}

const searchSoon = debounce(searchNow, 90);

function showMenu(x, y) {
  const width = 190;
  const height = 198;
  els.menu.style.left = `${Math.min(x, window.innerWidth - width - 4)}px`;
  els.menu.style.top = `${Math.min(y, window.innerHeight - height - 4)}px`;
  els.menu.classList.add("open");
}

function hideMenu() {
  els.menu.classList.remove("open");
}

async function openSelected() {
  const [path] = selectedPaths();
  if (path) await call("Open", path);
}

async function revealSelected() {
  const [path] = selectedPaths();
  if (path) await call("Reveal", path);
}

async function copySelected() {
  const paths = selectedPaths();
  if (paths.length) await call("CopyPaths", paths);
}

async function copyNameSelected() {
  const row = selectedRows()[0];
  if (row) await call("CopyPaths", [row.name]);
}

async function propsSelected() {
  const [path] = selectedPaths();
  if (path) await call("Properties", path);
}

async function renameSelected() {
  const row = selectedRows()[0];
  if (!row) return;
  const next = prompt("Rename", row.name);
  if (!next || next === row.name) return;
  await call("Rename", row.path, next);
  await searchNow();
}

async function deleteSelected() {
  const paths = selectedPaths();
  if (!paths.length) return;
  if (!confirm(`Move ${paths.length} selected item${paths.length === 1 ? "" : "s"} to the Recycle Bin?`)) return;
  await call("DeleteToRecycleBin", paths);
  await searchNow();
}

function moveSelection(delta) {
  if (!state.rows.length) return;
  const next = Math.max(0, Math.min(state.rows.length - 1, state.selected + delta));
  setSelected(next);
  const tr = els.results.querySelectorAll("tr")[next];
  if (tr) tr.scrollIntoView({ block: "nearest" });
}

els.query.addEventListener("input", searchSoon);
els.menu.addEventListener("click", async (event) => {
  const button = event.target.closest("button[data-action]");
  if (!button) return;
  hideMenu();
  const action = button.dataset.action;
  if (action === "open") await openSelected();
  if (action === "reveal") await revealSelected();
  if (action === "copy") await copySelected();
  if (action === "copy-name") await copyNameSelected();
  if (action === "rename") await renameSelected();
  if (action === "properties") await propsSelected();
  if (action === "delete") await deleteSelected();
});

document.addEventListener("click", hideMenu);
document.addEventListener("keydown", async (event) => {
  if (event.key === "Escape") {
    hideMenu();
  } else if (event.key === "ArrowDown") {
    event.preventDefault();
    moveSelection(1);
  } else if (event.key === "ArrowUp") {
    event.preventDefault();
    moveSelection(-1);
  } else if (event.key === "Enter") {
    event.preventDefault();
    await openSelected();
  } else if (event.key === "F2") {
    event.preventDefault();
    await renameSelected();
  } else if (event.key === "Delete") {
    event.preventDefault();
    await deleteSelected();
  } else if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "c") {
    event.preventDefault();
    await copySelected();
  }
});

setTimeout(refreshStatus, 150);
setInterval(refreshStatus, 10000);

waitForRuntime().then((runtime) => {
  if (runtime && runtime.EventsOn) {
    runtime.EventsOn("seekfs:search-results", handleSearchResponse);
  }
});
