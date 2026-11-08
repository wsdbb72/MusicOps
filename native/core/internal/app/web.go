package app

const appCSS = `:root{color-scheme:light;--ink:#1c1915;--muted:#5e584f;--line:#e4ddd2;--paper:#f7f4ef;--card:#fff;--accent:#0f6e56;--review:#8a4b08;--queue:#1d4e89}
*{box-sizing:border-box}body{margin:0;font:16px/1.5 "Segoe UI","PingFang SC","Noto Sans SC",sans-serif;color:var(--ink);background:var(--paper)}
header,main{padding:1.25rem 1.5rem}header{border-bottom:1px solid var(--line)}
h1,h2{font-size:1.35rem;font-weight:650;margin:.15rem 0}h2{font-size:1.1rem}.mark{margin:0;color:var(--accent);font-weight:700;letter-spacing:.04em}
nav{display:flex;flex-wrap:wrap;gap:.75rem 1rem;align-items:center;padding:.75rem 1.5rem;border-bottom:1px solid var(--line);background:#fff;position:sticky;top:0}
button{font:inherit;border:1px solid var(--line);background:#fff;color:var(--ink);border-radius:999px;padding:.4rem .85rem;cursor:pointer}
button[aria-current="page"],button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
button:disabled{opacity:.55;cursor:not-allowed}
.unavailable{display:inline-flex;align-items:center;gap:.4rem;color:var(--muted);font-size:.875rem}
#status{color:var(--muted);min-height:1.5rem}.cards{display:grid;gap:.75rem}
article{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:.9rem 1rem}
article button{border-radius:10px;padding:0;border:0;background:transparent;text-align:left;width:100%}
article h2{margin:0}.meta{display:flex;flex-wrap:wrap;gap:.4rem;margin:.45rem 0;color:var(--muted)}
.pill{border-radius:999px;padding:.05rem .55rem;background:#efeae3;color:var(--ink)}
.pill.review{background:#f8e7cf;color:var(--review)}.pill.queue{background:#e3eef8;color:var(--queue)}
dl{display:grid;grid-template-columns:7rem 1fr;gap:.35rem .75rem;margin:1rem 0}dt{color:var(--muted)}dd{margin:0}
#note{background:#fff;border:1px dashed var(--line);border-radius:14px;padding:1rem;color:var(--muted)}
@media(max-width:720px){header{display:block}nav{overflow:auto}dl{grid-template-columns:1fr}}
`

const appJS = `(function () {
  var titles = {tasks: "任务", queue: "队列", review: "核对", sites: "站点", library: "库", logs: "日志"};
  var active = {identifying: true, searching: true, requesting: true, downloading: true, fallback: true, validating: true, publishing: true};
  var view = "tasks";
  var jobs = [];
  var siteRows = [];
  var libraryRows = [];
  var logRows = [];
  var loading = false;
  var status = document.getElementById("status");
  var list = document.getElementById("list");
  var note = document.getElementById("note");
  var detail = document.getElementById("detail");
  var heading = document.querySelector("h1");

  function pill(job) {
    var moving = job.progress === "检索中" || job.progress === "获取中" || job.progress === "识别中" || job.progress === "已排队流媒体" || job.progress === "正在入库";
    var kind = job.progress === "待核对" ? "review" : (moving ? "queue" : "");
    return '<span class="pill ' + kind + '">' + escapeText(job.progress || "未下载") + "</span>";
  }
  function escapeText(value) {
    return String(value == null ? "" : value).replace(/[&<>"]/g, function (ch) {
      return {"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"}[ch];
    });
  }
  function roots() { return jobs.filter(function (job) { return !job.parent_job_id; }); }
  function visible() {
    if (view === "tasks") return roots();
    if (view === "queue") return jobs.filter(function (job) { return active[job.stage]; });
    if (view === "review") return jobs.filter(function (job) { return job.stage === "needs_review"; });
    return [];
  }
  function setBusy(next) {
    loading = next;
    document.querySelectorAll("#nav button[data-view], #back").forEach(function (button) { button.disabled = next; });
  }
  function render() {
    heading.textContent = titles[view];
    document.querySelectorAll("#nav button[data-view]").forEach(function (button) {
      button.setAttribute("aria-current", button.getAttribute("data-view") === view ? "page" : "false");
    });
    note.hidden = true;
    detail.hidden = true;
    list.hidden = false;
    if (view === "sites") { renderSites(); return; }
    if (view === "library") { renderLibrary(); return; }
    if (view === "logs") { renderLogs(); return; }
    var rows = visible();
    if (!rows.length) {
      list.innerHTML = "";
      status.textContent = view === "review" ? "没有待核对的任务。" : (view === "queue" ? "没有正在进行的任务。" : "还没有任务。");
      return;
    }
    status.textContent = "";
    var html = '<div class="cards">';
    rows.forEach(function (job) {
      html += '<article><button type="button" data-job="' + escapeText(job.job_id) + '"><h2>' + escapeText(job.label || "未命名") + '</h2><p class="meta">' + pill(job) + '</p><p>' + escapeText(job.headline || "") + "</p></button></article>";
    });
    list.innerHTML = html + "</div>";
  }
  function renderSites() {
    if (!siteRows.length) {
      list.innerHTML = "";
      status.textContent = "还没有站点设置。";
      return;
    }
    status.textContent = "";
    var html = '<div class="cards"><article><h2>默认站点</h2><p class="meta">检索请求里没有指定站点时，用这里的选择；每次检索也可以自己指定。</p>';
    siteRows.forEach(function (site) {
      html += '<label><input type="checkbox" data-site="' + escapeText(site.name) + '"' + (site.enabled ? " checked" : "") + "> " + escapeText(site.name) + "（" + escapeText(site.host || "") + "）</label><br>";
    });
    html += '<p><button type="button" id="save-sites" class="primary">保存</button></p></article></div>';
    list.innerHTML = html;
    var save = document.getElementById("save-sites");
    if (save) save.addEventListener("click", saveSites);
  }
  async function saveSites() {
    if (loading) return;
    var enabled = [];
    document.querySelectorAll("input[data-site]:checked").forEach(function (box) { enabled.push(box.getAttribute("data-site")); });
    setBusy(true);
    status.textContent = "正在保存";
    try {
      var response = await fetch("/v1/sites", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({enabled_sites: enabled})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        status.textContent = "保存失败。";
        return;
      }
      siteRows = Array.isArray(body.sites) ? body.sites : [];
      status.textContent = "已保存。";
      render();
    } catch (err) {
      status.textContent = "保存失败。";
    } finally {
      setBusy(false);
    }
  }
  function renderLibrary() {
    if (!libraryRows.length) {
      list.innerHTML = "";
      status.textContent = "库里还没有已入库的音频。";
      return;
    }
    status.textContent = "";
    var html = '<div class="cards">';
    libraryRows.forEach(function (entry) {
      html += '<article><h2>' + escapeText(entry.file || entry.source_name || "未命名") + '</h2><p class="meta"><span class="pill">' + escapeText(entry.format || "") + '</span><span class="pill">' + escapeText(entry.decode_state || "") + '</span><span class="pill">' + escapeText(entry.origin || "") + '</span></p><p>' + (entry.file_present ? "文件在库中" : "文件不在库中") + " · " + escapeText((entry.sha256 || "").slice(0, 12)) + "</p></article>";
    });
    list.innerHTML = html + "</div>";
  }
  function renderLogs() {
    if (!logRows.length) {
      list.innerHTML = "";
      status.textContent = "还没有运行记录。";
      return;
    }
    status.textContent = "";
    var html = '<div class="cards">';
    logRows.forEach(function (task) {
      html += '<article><h2>' + escapeText(task.task_id || "") + '</h2><p class="meta"><span class="pill">' + escapeText(task.state || "") + '</span><span class="pill">' + escapeText(task.command || "") + "</span></p><p>" + escapeText(task.message || "") + "</p></article>";
    });
    list.innerHTML = html + "</div>";
  }
  function field(name, value) {
    var row = document.createElement("div");
    var term = document.createElement("dt");
    var data = document.createElement("dd");
    term.textContent = name;
    data.textContent = value || "—";
    detailFields.appendChild(term);
    detailFields.appendChild(data);
  }
  var detailFields = document.getElementById("detail-fields");
  async function openJob(id) {
    if (loading) return;
    setBusy(true);
    status.textContent = "正在读取";
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(id), {headers: {Accept: "application/json"}});
      var body = await response.json();
      var job = body && body.inspection && body.inspection.job;
      if (!response.ok || !job) {
        status.textContent = "没有读到这条任务。";
        return;
      }
      list.hidden = true;
      note.hidden = true;
      detail.hidden = false;
      document.getElementById("detail-title").textContent = job.label || "未命名";
      detailFields.replaceChildren();
      field("进度", body.progress);
      field("说明", body.headline);
      field("下载", body.downloaded ? "已下载" : "未下载");
      field("入库", body.library_published ? "已入库" : "未入库");
      status.textContent = "";
    } catch (err) {
      status.textContent = "读取中断。";
    } finally {
      setBusy(false);
    }
  }
  async function load(quiet) {
    if (loading) return;
    if (!quiet) {
      setBusy(true);
      status.textContent = "正在读取";
    }
    try {
      if (view === "sites") {
        var siteResponse = await fetch("/v1/sites", {headers: {Accept: "application/json"}});
        var siteBody = await siteResponse.json();
        siteRows = siteBody && Array.isArray(siteBody.sites) ? siteBody.sites : [];
      } else if (view === "library") {
        var libraryResponse = await fetch("/v1/library", {headers: {Accept: "application/json"}});
        var libraryBody = await libraryResponse.json();
        libraryRows = libraryBody && Array.isArray(libraryBody.entries) ? libraryBody.entries : [];
      } else if (view === "logs") {
        var logResponse = await fetch("/v1/logs", {headers: {Accept: "application/json"}});
        var logBody = await logResponse.json();
        logRows = logBody && Array.isArray(logBody.tasks) ? logBody.tasks : [];
      } else {
        var response = await fetch("/v1/jobs", {headers: {Accept: "application/json"}});
        var body = await response.json();
        jobs = body && Array.isArray(body.jobs) ? body.jobs : [];
      }
      if (detail.hidden) render();
      arm();
    } catch (err) {
      if (!quiet) status.textContent = "任务列表没有读到。";
    } finally {
      if (!quiet) setBusy(false);
    }
  }
  var timer = 0;
  function arm() {
    clearTimeout(timer);
    if (view !== "tasks" && view !== "queue" && view !== "review") return;
    if (!jobs.some(function (job) { return active[job.stage]; })) return;
    timer = setTimeout(function () { load(true); }, 2000);
  }
  document.getElementById("nav").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (!button || !button.getAttribute("data-view") || loading) return;
    view = button.getAttribute("data-view");
    detail.hidden = true;
    load();
  });
  list.addEventListener("click", function (event) {
    var button = event.target.closest("button[data-job]");
    if (!button || loading) return;
    openJob(button.getAttribute("data-job"));
  });
  document.getElementById("back").addEventListener("click", function () {
    if (loading) return;
    detail.hidden = true;
    render();
  });
  load();
})();
`
