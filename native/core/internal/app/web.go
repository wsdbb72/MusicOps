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
#status{color:var(--muted);min-height:1.5rem}#status.ok{color:var(--accent)}#status.fail{color:#a32020}#status.review{color:var(--review)}
form label{display:block;margin:.6rem 0}form input[type="text"],form input[type="password"],form input[type="url"],form select,form textarea{width:100%;padding:.45rem .6rem;border:1px solid var(--line);border-radius:8px;font:inherit;background:#fff}form input[type="checkbox"]{width:auto}
.batch{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;margin-bottom:.75rem}
.review-box{border:1px solid var(--line);border-radius:14px;padding:1rem;background:#fff;margin-top:1rem}.review-box .candidate{display:block;margin:.4rem 0}.cards{display:grid;gap:.75rem}
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
  var titles = {tasks: "任务", queue: "队列", review: "核对", search: "搜索", sites: "站点", settings: "设置", library: "库", logs: "日志"};
  var active = {identifying: true, searching: true, requesting: true, downloading: true, fallback: true, validating: true, publishing: true};
  var view = "tasks";
  var jobs = [];
  var currentJob = null;
  var siteRows = [];
  var downloadSettings = null;
  var libraryRows = [];
  var transcodeRows = [];
  var logRows = [];
  var selectedJobs = {};
  var loading = false;
  var status = document.getElementById("status");
  var list = document.getElementById("list");
  var note = document.getElementById("note");
  var detail = document.getElementById("detail");
  var heading = document.querySelector("h1");

  // One feedback channel for every action: kind ok/fail/review/info, and the
  // server's code when the server refused, instead of a generic failure.
  function setStatus(kind, text) {
    status.className = kind || "";
    status.textContent = text;
  }

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
    if (view === "search") { renderSearch(); return; }
    if (view === "sites") { renderSites(); return; }
    if (view === "settings") { renderSettings(); return; }
    if (view === "library") { renderLibrary(); return; }
    if (view === "logs") { renderLogs(); return; }
    var rows = visible();
    if (!rows.length) {
      list.innerHTML = "";
      setStatus("", view === "review" ? "没有待核对的任务。" : (view === "queue" ? "没有正在进行的任务。" : "还没有任务。"));
      return;
    }
    setStatus("", "");
    var html = "";
    if (view === "tasks" || view === "queue") html += batchBar();
    html += '<div class="cards">';
    rows.forEach(function (job) {
      var checked = selectedJobs[job.job_id] ? " checked" : "";
      var box = (view === "tasks" || view === "queue") ? '<label><input type="checkbox" data-batch="' + escapeText(job.job_id) + '"' + checked + '> 批量</label>' : "";
      html += '<article>' + box + '<button type="button" data-job="' + escapeText(job.job_id) + '"><h2>' + escapeText(job.label || "未命名") + '</h2><p class="meta">' + pill(job) + '</p><p>' + escapeText(job.headline || "") + "</p></button></article>";
    });
    list.innerHTML = html + "</div>";
  }
  function selectedCount() {
    var count = 0;
    Object.keys(selectedJobs).forEach(function (id) { if (selectedJobs[id]) count++; });
    return count;
  }
  function batchBar() {
    var count = selectedCount();
    return '<div class="batch"><span>已选 ' + count + ' 个</span>' +
      '<button type="button" data-batch-action="pause">暂停</button>' +
      '<button type="button" data-batch-action="resume">继续</button>' +
      '<button type="button" data-batch-action="cancel">取消</button>' +
      '<button type="button" data-batch-action="retry">重试</button></div>';
  }
  async function batchAction(action) {
    var ids = Object.keys(selectedJobs).filter(function (id) { return selectedJobs[id]; });
    if (!ids.length) { setStatus("fail", "先勾选要批量操作的任务。"); return; }
    if (loading) return;
    if ((action === "cancel" || action === "retry") && !window.confirm(action === "cancel" ? "取消选中的 " + ids.length + " 个任务吗？取消后未开始的阶段不会继续。" : "重试选中的 " + ids.length + " 个任务吗？")) return;
    setBusy(true);
    setStatus("info", "正在批量处理");
    try {
      var response = await fetch("/v1/jobs/batch", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({job_ids: ids, action: action})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "批量操作失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      var failed = [];
      (body.results || []).forEach(function (item) { if (!item.ok) failed.push(item.job_id + "：" + (item.code || "失败")); });
      Object.keys(selectedJobs).forEach(function (id) { selectedJobs[id] = false; });
      if (failed.length) setStatus("review", "批量完成：成功 " + body.succeeded + " 个，失败 " + body.failed + " 个。" + failed.join("；"));
      else setStatus("ok", "批量完成：成功 " + body.succeeded + " 个。");
      load();
    } catch (err) {
      setStatus("fail", "批量操作中断。");
    } finally {
      setBusy(false);
    }
  }
  function renderSites() {
    if (!siteRows.length) {
      list.innerHTML = "";
      setStatus("", "还没有站点设置。");
      return;
    }
    setStatus("", "");
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
    setStatus("info", "正在保存");
    try {
      var response = await fetch("/v1/sites", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({enabled_sites: enabled})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "保存失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      siteRows = Array.isArray(body.sites) ? body.sites : [];
      setStatus("ok", "已保存。");
      render();
    } catch (err) {
      setStatus("fail", "保存失败。");
    } finally {
      setBusy(false);
    }
  }
  function renderSearch() {
    setStatus("", "");
    var html = '<div class="cards"><article><form id="search-form"><h2>提交搜索</h2>' +
      '<p class="meta">四站只读检索；是否下载由核对结果与身份证据决定，这里不直接下载。</p>' +
      '<label>检索关键词<input type="text" id="search-query" required></label>' +
      '<label>期望标题<input type="text" id="search-title" required></label>' +
      '<label>期望艺人<input type="text" id="search-artist"></label>' +
      '<label>场景<select id="search-scene"><option value="playlist">歌单</option><option value="complete">补全</option><option value="upgrade">音质升级</option><option value="fill">补缺</option><option value="expand">扩展</option></select></label>' +
      '<label>来源曲目 ID（歌单场景，逗号分隔）<input type="text" id="search-tracks"></label>' +
      '<label>流媒体回退<select id="search-streaming"><option value="">跟随设置</option><option value="true">开</option><option value="false">关</option></select></label>' +
      '<fieldset><legend>检索站点（都不选则用站点页的默认选择）</legend><span id="search-sites"></span></fieldset>' +
      '<fieldset><legend>身份证据（来自目录的才算确认；AI 或合成结果不能确认身份）</legend>' +
      '<label>证据来源<select id="search-identity-kind"><option value="catalog">目录</option><option value="api">接口</option><option value="website">网站</option></select></label>' +
      '<label>来源名称<input type="text" id="search-identity-source" placeholder="如 musicbrainz" value="musicbrainz"></label>' +
      '<label>来源条目 ID<input type="text" id="search-identity-subject" placeholder="如 MusicBrainz Release ID"></label>' +
      '<label>备注<input type="text" id="search-identity-payload" placeholder="可选，如核对过的曲目数"></label>' +
      '<label><input type="checkbox" id="search-identity-tracklist"> 已核对曲目清单</label>' +
      '<label><input type="checkbox" id="search-identity-confirmed"> 确认这是同一发行（须先核对曲目清单）</label></fieldset>' +
      '<label>本次检索的 Cookie（只用于这次检索，不保存）<input type="password" id="search-cookie" autocomplete="off"></label>' +
      '<label><input type="checkbox" id="search-synthetic"> 合成测试（仅开发联调用，需填测试服务地址）</label>' +
      '<label>测试服务地址<input type="text" id="search-base-url" placeholder="合成测试时必填"></label>' +
      '<p><button type="submit" class="primary">提交搜索</button></p></form></article></div>';
    list.innerHTML = html;
    var sitesBox = document.getElementById("search-sites");
    if (sitesBox) {
      var siteHtml = "";
      siteRows.forEach(function (site) {
        siteHtml += '<label><input type="checkbox" data-search-site="' + escapeText(site.name) + '"' + (site.enabled ? " checked" : "") + "> " + escapeText(site.name) + "</label><br>";
      });
      sitesBox.innerHTML = siteHtml || "还没有站点设置。";
    }
    var form = document.getElementById("search-form");
    if (form) form.addEventListener("submit", submitSearch);
  }
  async function submitSearch(event) {
    event.preventDefault();
    if (loading) return;
    var query = document.getElementById("search-query").value.trim();
    var title = document.getElementById("search-title").value.trim();
    if (!query || !title) { setStatus("fail", "检索关键词和期望标题都要填。"); return; }
    var confirmed = document.getElementById("search-identity-confirmed").checked;
    var tracklist = document.getElementById("search-identity-tracklist").checked;
    if (confirmed && !tracklist) { setStatus("fail", "确认身份前须先核对曲目清单。"); return; }
    var synthetic = document.getElementById("search-synthetic").checked;
    var baseURL = document.getElementById("search-base-url").value.trim();
    if (synthetic && !baseURL) { setStatus("fail", "合成测试需要填测试服务地址。"); return; }
    var tracks = document.getElementById("search-tracks").value.split(",").map(function (v) { return v.trim(); }).filter(function (v) { return v; });
    var scene = document.getElementById("search-scene").value;
    if (scene === "playlist" && !tracks.length) { setStatus("fail", "歌单场景需要至少一个来源曲目 ID。"); return; }
    var sites = [];
    document.querySelectorAll("input[data-search-site]:checked").forEach(function (box) { sites.push(box.getAttribute("data-search-site")); });
    var streaming = document.getElementById("search-streaming").value;
    var jobID = "web-" + Date.now().toString(36);
    var identity = {
      evidence_id: "web-ev-" + Date.now().toString(36),
      kind: document.getElementById("search-identity-kind").value,
      source: document.getElementById("search-identity-source").value.trim() || "manual",
      subject_id: document.getElementById("search-identity-subject").value.trim(),
      tracklist_verified: tracklist,
      identity_confirmed: confirmed,
      payload: document.getElementById("search-identity-payload").value.trim()
    };
    var body = {job_id: jobID, scene: scene, query: query, expected_title: title, expected_artist: document.getElementById("search-artist").value.trim(), source_track_ids: tracks, identity: identity, cookie: document.getElementById("search-cookie").value, synthetic: synthetic, base_url: baseURL};
    if (sites.length) body.enabled_sites = sites;
    if (streaming !== "") body.streaming_enabled = streaming === "true";
    setBusy(true);
    setStatus("info", "正在提交搜索");
    try {
      var response = await fetch("/v1/search", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)});
      var result = await response.json();
      if (!response.ok || !result || !result.ok) {
        setStatus("fail", "提交失败。" + (result && result.code ? "（" + result.code + "）" : ""));
        return;
      }
      setStatus("ok", "已提交搜索，任务 " + jobID + "。");
      view = "queue";
      load();
    } catch (err) {
      setStatus("fail", "提交中断。");
    } finally {
      setBusy(false);
    }
  }
  function renderSettings() {
    if (!downloadSettings) {
      setStatus("", "设置没有读到。");
      list.innerHTML = "";
      return;
    }
    setStatus("", "");
    var html = '<div class="cards"><article><h2>下载默认</h2><p class="meta">搜索时不指定流媒体回退，就用这里的选择。</p>' +
      '<label><input type="checkbox" id="download-streaming"' + (downloadSettings.streaming_enabled ? " checked" : "") + '> 流媒体回退（四站无精确匹配时走流媒体）</label>' +
      '<p><button type="button" id="save-download" class="primary">保存</button></p>' +
      '<p class="meta">流媒体回退顺序：' + (downloadSettings.streaming_sources || []).map(function (step) { return escapeText(step.name); }).join(" → ") + '</p>' +
      '<p class="meta">回退执行已接入：' + (downloadSettings.streaming_source_status || []).map(function (source) { return escapeText(source.name) + (source.configured ? "已接入" : "未接入"); }).join("、") + '。四站无凭证与引擎，歌单文件只导入曲目元数据。</p>' +
      '<p class="meta">各功能状态：下载已接入（需 qBittorrent 地址与登录，凭据只存内存）；目录核对已接入 MusicBrainz（Discogs 需用户 Token、网易云未接入）；媒体与库维护已接入（NCM 解密、FLAC 标签、歌词、去重、艺人文件夹；OpenCC 无字典数据，未接入）；转码已接入（需系统 ffmpeg/ffprobe）。</p></article>' +
      '<article><h2>站点登录检查</h2><p class="meta">只读检查：只验证登录与余额，不下载、不花 token。Cookie 只用于这次检查，不保存。</p>' +
      '<label>站点<select id="preflight-site">' + siteRows.map(function (site) { return '<option value="' + escapeText(site.name) + '">' + escapeText(site.name) + "</option>"; }).join("") + '</select></label>' +
      '<label>Cookie<input type="password" id="preflight-cookie" autocomplete="off"></label>' +
      '<label><input type="checkbox" id="preflight-synthetic"> 合成测试（需填测试服务地址）</label>' +
      '<label>测试服务地址<input type="text" id="preflight-base-url"></label>' +
      '<p><button type="button" id="run-preflight" class="primary">检查登录</button></p><p id="preflight-result" class="meta"></p></article></div>';
    list.innerHTML = html;
    var save = document.getElementById("save-download");
    if (save) save.addEventListener("click", saveDownload);
    var run = document.getElementById("run-preflight");
    if (run) run.addEventListener("click", preflight);
  }
  async function saveDownload() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var response = await fetch("/v1/settings/download", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({streaming_enabled: document.getElementById("download-streaming").checked})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "保存失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      downloadSettings.streaming_enabled = body.streaming_enabled;
      setStatus("ok", "已保存。");
    } catch (err) {
      setStatus("fail", "保存失败。");
    } finally {
      setBusy(false);
    }
  }
  async function preflight() {
    if (loading) return;
    var cookie = document.getElementById("preflight-cookie").value;
    if (!cookie) { setStatus("fail", "先填 Cookie。"); return; }
    var synthetic = document.getElementById("preflight-synthetic").checked;
    var baseURL = document.getElementById("preflight-base-url").value.trim();
    if (synthetic && !baseURL) { setStatus("fail", "合成测试需要填测试服务地址。"); return; }
    setBusy(true);
    setStatus("info", "正在检查登录");
    try {
      var response = await fetch("/v1/sites/preflight", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({site: document.getElementById("preflight-site").value, cookie: cookie, synthetic: synthetic, base_url: baseURL})});
      var body = await response.json();
      var result = document.getElementById("preflight-result");
      if (!response.ok || !body || !body.ok) {
        if (result) result.textContent = "检查失败。" + (body && body.code ? "（" + body.code + "）" : "");
        setStatus("fail", "检查失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      var text = body.site + "：" + body.status + "，登录" + (body.authenticated ? "成功" : "失败");
      if (body.token_balance != null) text += "，余额 " + body.token_balance;
      if (body.reason) text += "，" + body.reason;
      if (result) result.textContent = text;
      setStatus(body.authenticated ? "ok" : "review", text + "。");
    } catch (err) {
      setStatus("fail", "检查中断。");
    } finally {
      setBusy(false);
    }
  }
  function renderLibrary() {
    var html = '<div class="cards"><article><form id="transcode-form"><h2>转码为 FLAC</h2>' +
      '<label>源文件（库内 WAV 路径，应用数据目录内）<input type="text" id="transcode-source" required placeholder="如 library-audio/Artist/Album/01 Title.wav"></label>' +
      '<p><button type="submit" class="primary">开始转码</button></p><p class="meta">只支持 PCM WAV 转 FLAC；转码前后 PCM 内容逐字节比对，源文件不动，输出已存在时拒绝覆盖。</p></form></article>';
    if (transcodeRows.length) {
      html += '<article><h2>转码记录</h2>';
      transcodeRows.forEach(function (receipt) {
        html += '<p>' + escapeText(receipt.request_id || "") + " · " + escapeText(receipt.format || "") + " · " + escapeText((receipt.sha256 || "").slice(0, 12)) + "<br>" + escapeText(receipt.output || "") + "</p>";
      });
      html += "</article>";
    }
    if (!libraryRows.length) {
      list.innerHTML = html + "</div>";
      setStatus("", "库里还没有已入库的音频。");
      var form = document.getElementById("transcode-form");
      if (form) form.addEventListener("submit", submitTranscode);
      return;
    }
    setStatus("", "");
    libraryRows.forEach(function (entry) {
      html += '<article><h2>' + escapeText(entry.file || entry.source_name || "未命名") + '</h2><p class="meta"><span class="pill">' + escapeText(entry.format || "") + '</span><span class="pill">' + escapeText(entry.decode_state || "") + '</span><span class="pill">' + escapeText(entry.origin || "") + '</span></p><p>' + (entry.file_present ? "文件在库中" : "文件不在库中") + " · " + escapeText((entry.sha256 || "").slice(0, 12)) + "</p></article>";
    });
    list.innerHTML = html + "</div>";
    var form = document.getElementById("transcode-form");
    if (form) form.addEventListener("submit", submitTranscode);
  }
  async function submitTranscode(event) {
    event.preventDefault();
    if (loading) return;
    var source = document.getElementById("transcode-source");
    if (!source || !source.value.trim()) { setStatus("fail", "先填源文件路径。"); return; }
    setBusy(true);
    setStatus("info", "正在提交转码");
    try {
      var response = await fetch("/v1/library/transcode", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({request_id: "tc-" + Date.now().toString(36), source: source.value.trim(), format: "flac"})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "转码提交失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      setStatus("ok", "已提交转码，完成后在转码记录里查看。");
    } catch (err) {
      setStatus("fail", "转码提交中断。");
    } finally {
      setBusy(false);
    }
    load(true);
  }
  function renderLogs() {
    if (!logRows.length) {
      list.innerHTML = "";
      setStatus("", "还没有运行记录。");
      return;
    }
    setStatus("", "");
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
    setStatus("info", "正在读取");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(id), {headers: {Accept: "application/json"}});
      var body = await response.json();
      var job = body && body.inspection && body.inspection.job;
      if (!response.ok || !job) {
        setStatus("fail", "没有读到这条任务。");
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
      currentJob = job;
      showActions(job);
      if (job.stage === "needs_review") openReview(job.job_id);
      else document.getElementById("review-box").hidden = true;
      setStatus("", "");
    } catch (err) {
      setStatus("fail", "读取中断。");
    } finally {
      setBusy(false);
    }
  }
  function showActions(job) {
    var terminal = job.stage === "completed" || job.stage === "failed" || job.stage === "cancelled" || job.stage === "skipped";
    var needsReview = job.stage === "needs_review";
    // Pause/resume/cancel always refuse a blocked needs_review job, so hide
    // them there; the review box carries the only way out.
    document.getElementById("pause-job").hidden = terminal || needsReview || !!job.paused;
    document.getElementById("resume-job").hidden = terminal || needsReview || !job.paused;
    document.getElementById("cancel-job").hidden = terminal || needsReview || !!job.cancel_requested;
    document.getElementById("retry-job").hidden = job.stage !== "failed";
  }
  async function openReview(id) {
    var box = document.getElementById("review-box");
    box.hidden = false;
    box.innerHTML = "<p>正在读取核对内容</p>";
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(id) + "/review", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        box.innerHTML = "<p>核对内容没有读到。" + (body && body.code ? "（" + escapeText(body.code) + "）" : "") + "</p>";
        return;
      }
      var html = "<h3>待核对</h3><p>" + escapeText(body.message || "") + "</p><p class=\"meta\">期望：" + escapeText(body.expected_title || "") + (body.expected_artist ? " · " + escapeText(body.expected_artist) : "") + "</p>";
      html += "<p class=\"meta\">" + (body.sites || []).map(function (site) { return escapeText(site.site) + "：" + escapeText(site.status) + "（" + site.candidate_count + " 个结果" + (site.exact ? "，有精确匹配" : "") + (site.limited ? "，未搜全" : "") + ")"; }).join("<br>") + "</p>";
      var candidates = body.candidates || [];
      if (candidates.length) {
        html += "<p>候选（只列出已观测到的结果）：</p>";
        candidates.forEach(function (candidate, index) {
          html += '<label class="candidate"><input type="radio" name="review-candidate" value="' + escapeText(candidate.candidate_id) + '" data-site="' + escapeText(candidate.site) + '"' + (index === 0 ? " checked" : "") + "> " + escapeText(candidate.title) + " · " + escapeText((candidate.artists || []).join(" / ")) + " · " + escapeText(candidate.site) + (candidate.seeders != null ? " · 做种 " + candidate.seeders : "") + "</label>";
        });
      } else {
        html += "<p>没有观测到的候选。</p>";
      }
      var allowed = {};
      (body.actions || []).forEach(function (action) { allowed[action.action] = action; });
      html += '<p id="review-actions">';
      if (allowed.accept && allowed.accept.allowed) html += '<button type="button" id="review-accept" class="primary">接受选中</button> ';
      else if (allowed.accept) html += '<span class="meta">接受不可用：' + escapeText(allowed.accept.reason || "") + "</span> ";
      if (allowed.retry && allowed.retry.allowed) html += '<button type="button" id="review-retry">重试</button> ';
      else if (allowed.retry) html += '<span class="meta">重试不可用：' + escapeText(allowed.retry.reason || "") + "</span> ";
      if (allowed.skip && allowed.skip.allowed) html += '<button type="button" id="review-skip">跳过</button>';
      else if (allowed.skip) html += '<span class="meta">跳过不可用：' + escapeText(allowed.skip.reason || "") + "</span>";
      html += "</p>";
      box.innerHTML = html;
      var accept = document.getElementById("review-accept");
      if (accept) accept.addEventListener("click", function () { reviewAction("accept"); });
      var retry = document.getElementById("review-retry");
      if (retry) retry.addEventListener("click", function () { reviewAction("retry"); });
      var skip = document.getElementById("review-skip");
      if (skip) skip.addEventListener("click", function () { reviewAction("skip"); });
    } catch (err) {
      box.innerHTML = "<p>核对内容读取中断。</p>";
    }
  }
  async function reviewAction(action) {
    if (!currentJob || loading) return;
    var message = action === "skip" ? "跳过这条任务吗？跳过后不再继续处理。" : (action === "retry" ? "重试这条任务吗？" : "");
    if (message && !window.confirm(message)) return;
    var site = "";
    var candidateID = "";
    if (action === "accept") {
      var picked = document.querySelector("input[name=\"review-candidate\"]:checked");
      if (!picked) { setStatus("fail", "先选一个候选。"); return; }
      site = picked.getAttribute("data-site");
      candidateID = picked.value;
    }
    setBusy(true);
    setStatus("info", "正在处理核对");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentJob.job_id) + "/review", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: action, site: site, candidate_id: candidateID})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "核对操作失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      setStatus(action === "skip" ? "review" : "ok", action === "accept" ? "已接受候选，继续处理。" : (action === "retry" ? "已重试。" : "已跳过。"));
    } catch (err) {
      setStatus("fail", "核对操作中断。");
    } finally {
      setBusy(false);
    }
    openJob(currentJob.job_id);
  }
  async function controlJob(action, message) {
    if (!currentJob || loading) return;
    if (message && !window.confirm(message)) return;
    setBusy(true);
    setStatus("info", "正在处理");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentJob.job_id) + "/control", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: action})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "操作失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      setStatus("ok", action === "pause" ? "已暂停。" : (action === "resume" ? "已继续。" : "已取消。"));
    } catch (err) {
      setStatus("fail", "操作失败。");
    } finally {
      setBusy(false);
    }
    openJob(currentJob.job_id);
  }
  async function retryJob() {
    if (!currentJob || loading) return;
    if (!window.confirm("重试这条任务吗？")) return;
    setBusy(true);
    setStatus("info", "正在重试");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentJob.job_id) + "/review", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: "retry"})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "重试失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      setStatus("ok", "已重试。");
    } catch (err) {
      setStatus("fail", "重试中断。");
    } finally {
      setBusy(false);
    }
    openJob(currentJob.job_id);
  }
  async function load(quiet) {
    if (loading) return;
    if (!quiet) {
      setBusy(true);
      setStatus("info", "正在读取");
    }
    try {
      if (view === "sites" || view === "search" || view === "settings") {
        var siteResponse = await fetch("/v1/sites", {headers: {Accept: "application/json"}});
        var siteBody = await siteResponse.json();
        siteRows = siteBody && Array.isArray(siteBody.sites) ? siteBody.sites : [];
      }
      if (view === "settings") {
        var downloadResponse = await fetch("/v1/settings/download", {headers: {Accept: "application/json"}});
        var downloadBody = await downloadResponse.json();
        downloadSettings = downloadBody && downloadBody.ok ? downloadBody : null;
      } else if (view === "library") {
        var libraryResponse = await fetch("/v1/library", {headers: {Accept: "application/json"}});
        var libraryBody = await libraryResponse.json();
        libraryRows = libraryBody && Array.isArray(libraryBody.entries) ? libraryBody.entries : [];
        var transcodeResponse = await fetch("/v1/library/transcode", {headers: {Accept: "application/json"}});
        var transcodeBody = await transcodeResponse.json();
        transcodeRows = transcodeBody && Array.isArray(transcodeBody.receipts) ? transcodeBody.receipts : [];
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
      if (!quiet) setStatus("fail", "任务列表没有读到。");
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
    var batchButton = event.target.closest("button[data-batch-action]");
    if (batchButton && !loading) { batchAction(batchButton.getAttribute("data-batch-action")); return; }
    var button = event.target.closest("button[data-job]");
    if (!button || loading) return;
    openJob(button.getAttribute("data-job"));
  });
  list.addEventListener("change", function (event) {
    var box = event.target.closest("input[data-batch]");
    if (!box) return;
    selectedJobs[box.getAttribute("data-batch")] = box.checked;
    var bar = document.querySelector(".batch span");
    if (bar) bar.textContent = "已选 " + selectedCount() + " 个";
  });
  document.getElementById("back").addEventListener("click", function () {
    if (loading) return;
    detail.hidden = true;
    render();
  });
  document.getElementById("pause-job").addEventListener("click", function () { controlJob("pause", ""); });
  document.getElementById("resume-job").addEventListener("click", function () { controlJob("resume", ""); });
  document.getElementById("cancel-job").addEventListener("click", function () { controlJob("cancel", "取消后未开始的阶段不会继续，确定取消吗？"); });
  document.getElementById("retry-job").addEventListener("click", retryJob);
  load();
})();
`
