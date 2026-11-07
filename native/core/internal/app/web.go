package app

const appCSS = `:root{color-scheme:light;--ink:#1c1915;--muted:#5e584f;--line:#e4ddd2;--paper:#f7f4ef;--card:#fff;--accent:#0f6e56;--review:#8a4b08;--queue:#1d4e89}
*{box-sizing:border-box}body{margin:0;font:16px/1.5 "Segoe UI","PingFang SC","Noto Sans SC",sans-serif;color:var(--ink);background:var(--paper)}
header,main{padding:1.25rem 1.5rem}header{display:flex;justify-content:space-between;gap:1rem;align-items:end;border-bottom:1px solid var(--line)}
h1,h2{font-size:1.35rem;font-weight:650;margin:.15rem 0}h2{font-size:1.1rem}.mark{margin:0;color:var(--accent);font-weight:700;letter-spacing:.04em}.quiet{margin:0;color:var(--muted);max-width:36rem}
nav{display:flex;gap:.5rem;padding:.75rem 1.5rem;border-bottom:1px solid var(--line);background:#fff;position:sticky;top:0}
button{font:inherit;border:1px solid var(--line);background:#fff;color:var(--ink);border-radius:999px;padding:.4rem .85rem;cursor:pointer}
button[aria-current="page"],button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
button:disabled{opacity:.55;cursor:wait}
#status{color:var(--muted);min-height:1.5rem}.cards{display:grid;gap:.75rem}
article{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:.9rem 1rem}
article button{border-radius:10px;padding:0;border:0;background:transparent;text-align:left;width:100%}
article h2{margin:0}.meta{display:flex;flex-wrap:wrap;gap:.4rem;margin:.45rem 0;color:var(--muted)}
.pill{border-radius:999px;padding:.05rem .55rem;background:#efeae3;color:var(--ink)}
.pill.review{background:#f8e7cf;color:var(--review)}.pill.queue{background:#e3eef8;color:var(--queue)}
dl{display:grid;grid-template-columns:7rem 1fr;gap:.35rem .75rem;margin:1rem 0}dt{color:var(--muted)}dd{margin:0}
#note{background:#fff;border:1px dashed var(--line);border-radius:14px;padding:1rem;color:var(--muted)}
@media(max-width:720px){header{display:block}header p.quiet{margin-top:.6rem}nav{overflow:auto}dl{grid-template-columns:1fr}}
`

const appJS = `(function () {
  var titles = {tasks: "任务", queue: "队列", review: "核对", sites: "站点", library: "库", log: "日志"};
  var stages = {identifying: "识别中", searching: "检索中", requesting: "获取中", needs_review: "待核对", downloading: "下载中", fallback: "已排队流媒体", skipped: "已跳过", completed: "已完成"};
  var active = {identifying: true, searching: true, requesting: true, downloading: true, fallback: true};
  var view = "tasks";
  var jobs = [];
  var loading = false;
  var status = document.getElementById("status");
  var list = document.getElementById("list");
  var note = document.getElementById("note");
  var detail = document.getElementById("detail");
  var heading = document.querySelector("h1");

  function stageText(value) { return stages[value] || value || "未知"; }
  function pill(job) {
    var kind = job.stage === "needs_review" ? "review" : (job.stage === "identifying" || job.stage === "searching" || job.stage === "requesting" ? "queue" : "");
    return '<span class="pill ' + kind + '">' + escapeText(stageText(job.stage)) + "</span>";
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
    if (view === "log") return jobs.slice().sort(function (a, b) { return (b.updated_ns || 0) - (a.updated_ns || 0); });
    return [];
  }
  function setBusy(next) {
    loading = next;
    document.querySelectorAll("#nav button, #back").forEach(function (button) { button.disabled = next; });
  }
  function showNote(text) {
    list.hidden = true;
    detail.hidden = true;
    note.hidden = false;
    note.textContent = text;
  }
  function render() {
    heading.textContent = titles[view];
    document.querySelectorAll("#nav button").forEach(function (button) {
      button.setAttribute("aria-current", button.getAttribute("data-view") === view ? "page" : "false");
    });
    if (view === "sites") {
      showNote("四个站点的只读检索由这个进程执行。Cookie、authkey 和 torrent_pass 只留在一次获取的内存里，页面不回显。流媒体获取和令牌扣费确认还没有接到这里。这里不显示编造的余额。");
      status.textContent = "";
      return;
    }
    if (view === "library") {
      showNote("用户音乐库发布还没有执行器。已经保存的种子字节不是专辑文件，也不会出现在这里。");
      status.textContent = "";
      return;
    }
    note.hidden = true;
    detail.hidden = true;
    list.hidden = false;
    var rows = visible();
    if (!rows.length) {
      list.innerHTML = "";
      status.textContent = view === "review" ? "没有待核对的任务。" : (view === "queue" ? "没有进行中的任务。" : "还没有任务。");
      return;
    }
    status.textContent = rows.length + " 条。这些记录来自本进程，不是站点回执。";
    var html = '<div class="cards">';
    rows.forEach(function (job) {
      var waiting = jobs.some(function (item) { return item.parent_job_id === job.job_id && item.stage === "needs_review"; });
      var extra = waiting ? '<span class="pill review">待核对</span>' : "";
      html += '<article><button type="button" data-job="' + escapeText(job.job_id) + '"><h2>' + escapeText(job.label || job.job_id) + '</h2><p class="meta">' + pill(job) + extra + "<span>" + escapeText(job.scene || "") + "</span><span>" + escapeText(job.error_class || "") + '</span></p><p>' + escapeText(job.message || "") + "</p></button></article>";
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
      document.getElementById("detail-title").textContent = job.label || job.job_id;
      detailFields.replaceChildren();
      field("编号", job.job_id);
      field("阶段", stageText(job.stage));
      field("核对", job.review_kind);
      field("结果类", job.error_class);
      field("说明", job.message);
      field("已下载", "否");
      field("已入库", "否");
      status.textContent = "";
    } catch (err) {
      status.textContent = "读取中断。";
    } finally {
      setBusy(false);
    }
  }
  async function load() {
    if (loading) return;
    setBusy(true);
    status.textContent = "正在读取";
    try {
      var response = await fetch("/v1/jobs", {headers: {Accept: "application/json"}});
      var body = await response.json();
      jobs = body && Array.isArray(body.jobs) ? body.jobs : [];
      render();
    } catch (err) {
      status.textContent = "任务列表没有读到。";
    } finally {
      setBusy(false);
    }
  }
  document.getElementById("nav").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (!button || loading) return;
    view = button.getAttribute("data-view");
    detail.hidden = true;
    render();
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
