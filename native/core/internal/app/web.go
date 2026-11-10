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
  var cookieSettings = [];
  var aiSettings = null;
  var qbtSettings = null;
  var solverSettings = null;
  var lyricsSettings = null;
  var discogsSettings = null;
  var librarySettings = null;
  var migrationReport = null;
  var logEvents = [];
  var serverSettings = null;
  var playlistRows = [];
  var subscriptionRows = [];
  var streamingOrder = [];
  var libraryRows = [];
  var duplicateRows = [];
  var libraryFileOffset = 0;
  var selectedTracks = {};
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
      list.innerHTML = (view === "tasks" || view === "queue") ? pipelineBar() : "";
      if (view === "tasks" || view === "queue") bindPipeline();
      setStatus("", view === "review" ? "没有待核对的任务。" : (view === "queue" ? "没有正在进行的任务。" : "还没有任务。"));
      return;
    }
    setStatus("", "");
    var html = "";
    if (view === "tasks" || view === "queue") html += pipelineBar() + batchBar();
    html += '<div class="cards">';
    rows.forEach(function (job) {
      var checked = selectedJobs[job.job_id] ? " checked" : "";
      var box = (view === "tasks" || view === "queue") ? '<label><input type="checkbox" data-batch="' + escapeText(job.job_id) + '"' + checked + '> 批量</label>' : "";
      html += '<article>' + box + '<button type="button" data-job="' + escapeText(job.job_id) + '"><h2>' + escapeText(job.label || "未命名") + '</h2><p class="meta">' + pill(job) + '</p><p>' + escapeText(job.headline || "") + "</p></button></article>";
    });
    list.innerHTML = html + "</div>";
    if (view === "tasks" || view === "queue") bindPipeline();
  }
  function pipelineBar() {
    return '<p><button type="button" id="pipeline-pause">暂停全部后台</button> <button type="button" id="pipeline-resume">继续后台</button></p>';
  }
  function bindPipeline() {
    var pause = document.getElementById("pipeline-pause");
    var resume = document.getElementById("pipeline-resume");
    if (pause) pause.addEventListener("click", function () { postSetting("/v1/control", {action: "pause"}).then(function () { setStatus("ok", "后台已暂停。正在跑的检索会停在下一步，不会新扣令牌。"); }); });
    if (resume) resume.addEventListener("click", function () { postSetting("/v1/control", {action: "resume"}).then(function () { setStatus("ok", "后台已继续。"); }); });
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
      '<label>已存歌单（歌单场景：回退时读该歌单文件）<select id="search-playlist"><option value="">不使用已存歌单</option>' + playlistRows.map(function (row) { return '<option value="' + escapeText(row.id) + '">' + escapeText((row.name || row.id) + "（" + row.track_count + " 首）") + "</option>"; }).join("") + '</select></label>' +
      '<label>流媒体回退<select id="search-streaming"><option value="">跟随设置</option><option value="true">开</option><option value="false">关</option></select></label>' +
      '<fieldset><legend>检索站点（都不选则用站点页的默认选择）</legend><span id="search-sites"></span></fieldset>' +
      '<fieldset><legend>身份证据（来自目录的才算确认；AI 或合成结果不能确认身份）</legend>' +
      '<label>证据来源<select id="search-identity-kind"><option value="catalog">目录</option><option value="api">接口</option><option value="website">网站</option></select></label>' +
      '<label>来源名称<input type="text" id="search-identity-source" placeholder="如 musicbrainz" value="musicbrainz"></label>' +
      '<label>来源条目 ID<input type="text" id="search-identity-subject" placeholder="如 MusicBrainz Release ID"></label>' +
      '<label>备注<input type="text" id="search-identity-payload" placeholder="可选，如核对过的曲目数"></label>' +
      '<label><input type="checkbox" id="search-identity-tracklist"> 已核对曲目清单</label>' +
      '<label><input type="checkbox" id="search-identity-confirmed"> 确认这是同一发行（须先核对曲目清单）</label></fieldset>' +
      '<label>本次检索的 Cookie（只用于这次检索，不保存；留空用已保存的 Cookie）<input type="password" id="search-cookie" autocomplete="off"></label>' +
      '<label><input type="checkbox" id="search-synthetic"> 使用测试服务器（不访问真实站点）</label>' +
      '<label>测试服务器地址<input type="text" id="search-base-url" placeholder="使用测试服务器时必填"></label>' +
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
    if (synthetic && !baseURL) { setStatus("fail", "使用测试服务器时需要填写地址。"); return; }
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
    var playlistID = document.getElementById("search-playlist").value;
    if (playlistID) body.playlist_id = playlistID;
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
  async function postSetting(path, body) {
    var response = await fetch(path, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)});
    var result = await response.json();
    if (!response.ok || !result || !result.ok) throw new Error((result && (result.error || result.code)) || "请求失败");
    return result;
  }
  function savedMark(saved, revision) {
    return saved ? "已保存" + (revision ? "（版本 " + revision + "）" : "") : "未保存";
  }
  function moveOrder(index, delta) {
    var next = index + delta;
    if (next < 0 || next >= streamingOrder.length) return;
    var tmp = streamingOrder[index];
    streamingOrder[index] = streamingOrder[next];
    streamingOrder[next] = tmp;
    renderSettings();
  }
  function downloadCard() {
    if (!downloadSettings) return "";
    var orderHtml = streamingOrder.map(function (name, index) {
      return '<p class="meta">' + escapeText(name) +
        ' <button type="button" data-order-move="' + index + ':-1">↑</button>' +
        ' <button type="button" data-order-move="' + index + ':1">↓</button></p>';
    }).join("");
    return '<article><h2>下载默认</h2><p class="meta">搜索时不指定流媒体回退，就用这里的选择。</p>' +
      '<label><input type="checkbox" id="download-streaming"' + (downloadSettings.streaming_enabled ? " checked" : "") + '> 流媒体回退（四站无精确匹配时走流媒体）</label>' +
      '<label>本机 go-music-dl 地址<input type="text" id="download-engine" value="' + escapeText(downloadSettings.engine_url || "") + '" placeholder="留空则未接入。只接受本机 HTTP 或 HTTPS"></label>' +
      '<p><button type="button" id="save-download" class="primary">保存</button></p>' +
      '<p class="meta">流媒体回退顺序（可上下调整）：</p>' + orderHtml +
      '<p class="meta">回退顺序默认先 Tidal、Apple Music，再 QQ 音乐、网易云，最后是已导入的歌单文件。go-music-dl 是 AGPL-3.0，不会打进这个程序；只有用户本机服务返回并通过音频校验的文件才算下载，而且仍未入库。</p></article>';
  }
  function cookieCard() {
    var rows = cookieSettings.map(function (site) {
      return '<label>' + escapeText(site.name) + '（' + escapeText(site.host || "") + '）· ' + escapeText(savedMark(site.saved, site.revision)) +
        '<textarea id="cookie-' + escapeText(site.name) + '" rows="3" placeholder="粘贴 Cookie-Editor JSON；留空保留已保存的值"></textarea></label>' +
        '<p><button type="button" data-cookie-site="' + escapeText(site.name) + '">保存 ' + escapeText(site.name) + ' Cookie</button></p>';
    }).join("");
    return '<article><h2>四站 Cookie</h2><p class="meta">每站的 Cookie 加密存在 Vault 里，只显示“已保存”，不回显内容。搜索、预检、下载时优先用这里保存的 Cookie；检索表单里现填的 Cookie 优先于已存值。</p>' + rows + '</article>';
  }
  function aiCard() {
    if (!aiSettings) return "";
    return '<article><h2>AI 设置</h2><p class="meta">核对模型：启用后，待核对的候选会走这里的模型再核对一次；模型结论仍要过本地校验才能通过。API Key 只存 Vault，不回显。</p>' +
      '<label><input type="checkbox" id="ai-enabled"' + (aiSettings.enabled ? " checked" : "") + '> 启用 AI 核对</label>' +
      '<label>AI 地址<input type="text" id="ai-base-url" value="' + escapeText(aiSettings.base_url || "") + '" placeholder="如 https://ai.example.com/v1"></label>' +
      '<label>模型<input type="text" id="ai-model" value="' + escapeText(aiSettings.model || "") + '"></label>' +
      '<label>API Key（' + escapeText(savedMark(aiSettings.key_saved, aiSettings.key_revision)) + '，留空保留）<input type="password" id="ai-key" autocomplete="off"></label>' +
      '<label>超时（秒）<input type="text" id="ai-timeout" value="' + escapeText(aiSettings.timeout) + '"></label>' +
      '<label>最大输出 token<input type="text" id="ai-max-tokens" value="' + escapeText(aiSettings.max_output_tokens) + '"></label>' +
      '<label>温度<input type="text" id="ai-temperature" value="' + escapeText(aiSettings.temperature) + '"></label>' +
      '<p><button type="button" id="save-ai" class="primary">保存</button> <button type="button" id="test-ai">测试连接</button></p><p id="ai-test-result" class="meta"></p></article>';
  }
  function qbtCard() {
    if (!qbtSettings) return "";
    return '<article><h2>qBittorrent</h2><p class="meta">下载用的 qBittorrent 地址与登录。登录只存 Vault，不回显；用户名和密码须一起填，密码留空保留已保存的登录。</p>' +
      '<label>地址<input type="text" id="qbt-base-url" value="' + escapeText(qbtSettings.base_url || "") + '" placeholder="http://127.0.0.1:8080"></label>' +
      '<label>用户名<input type="text" id="qbt-username" autocomplete="off"></label>' +
      '<label>密码（' + escapeText(savedMark(qbtSettings.login_saved, qbtSettings.login_revision)) + '，留空保留）<input type="password" id="qbt-password" autocomplete="off"></label>' +
      '<label>分类<input type="text" id="qbt-category" value="' + escapeText(qbtSettings.category || "") + '"></label>' +
      '<label>标签<input type="text" id="qbt-tags" value="' + escapeText(qbtSettings.tags || "") + '"></label>' +
      '<p><button type="button" id="save-qbt" class="primary">保存</button> <button type="button" id="test-qbt">测试连接</button> <button type="button" id="unify-qbt-tags">统一旧标签</button></p><p id="qbt-test-result" class="meta"></p></article>';
  }
  function solverCard() {
    if (!solverSettings) return "";
    return '<article><h2>FlareSolverr</h2><p class="meta">浏览器验证服务。地址须为本机回环地址。目前设置可保存、可测试，但请求还不走 solver——启用也不会改变任何请求行为。</p>' +
      '<label><input type="checkbox" id="solver-managed"' + (solverSettings.managed ? " checked" : "") + '> 由引擎管理（用内置 FlareSolverr）</label>' +
      '<label><input type="checkbox" id="solver-enabled"' + (solverSettings.enabled ? " checked" : "") + '> 启用浏览器验证</label>' +
      '<label>地址<input type="text" id="solver-base-url" value="' + escapeText(solverSettings.base_url || "") + '"></label>' +
      '<label>超时（秒，10–120）<input type="text" id="solver-timeout" value="' + escapeText(solverSettings.timeout) + '"></label>' +
      '<label>可执行程序（仅 Windows，.exe 绝对路径）<input type="text" id="solver-executable" value="' + escapeText(solverSettings.executable || "") + '"></label>' +
      '<label><input type="checkbox" id="solver-auto-start"' + (solverSettings.auto_start ? " checked" : "") + '> 首次请求时自动启动</label>' +
      '<p><button type="button" id="save-solver" class="primary">保存</button> <button type="button" id="test-solver">测试连接</button></p><p id="solver-test-result" class="meta"></p></article>';
  }
  function lyricsCard() {
    if (!lyricsSettings) return "";
    return '<article><h2>歌词与封面服务</h2><p class="meta">主服务须为 HTTPS；鉴权值是原始值，不补 Bearer 前缀，只存 Vault。设置可保存、可测试，但目前还没有拉取歌词/封面的客户端。</p>' +
      '<label>服务地址<input type="text" id="lyrics-base-url" value="' + escapeText(lyricsSettings.base_url || "") + '"></label>' +
      '<label>歌词路径<input type="text" id="lyrics-path" value="' + escapeText(lyricsSettings.lyrics_path || "") + '"></label>' +
      '<label>候选路径<input type="text" id="lyrics-candidates-path" value="' + escapeText(lyricsSettings.candidates_path || "") + '"></label>' +
      '<label>封面路径<input type="text" id="lyrics-cover-path" value="' + escapeText(lyricsSettings.cover_path || "") + '"></label>' +
      '<label>封面写入<select id="lyrics-write-mode"><option value="sidecar"' + (lyricsSettings.write_mode === "embedded" ? "" : " selected") + '>内嵌并保留同名 jpg</option><option value="embedded"' + (lyricsSettings.write_mode === "embedded" ? " selected" : "") + '>只写入 FLAC 内嵌图片</option></select></label>' +
      '<label>鉴权请求头<input type="text" id="lyrics-auth-header" value="' + escapeText(lyricsSettings.auth_header || "") + '"></label>' +
      '<label>鉴权值（' + escapeText(savedMark(lyricsSettings.auth_saved, lyricsSettings.auth_revision)) + '，留空保留）<input type="password" id="lyrics-auth" autocomplete="off"></label>' +
      '<p><button type="button" id="save-lyrics" class="primary">保存</button> <button type="button" id="test-lyrics">测试连接</button></p><p id="lyrics-test-result" class="meta"></p></article>';
  }
  function discogsCard() {
    if (!discogsSettings) return "";
    return '<article><h2>Discogs</h2><p class="meta">在线专辑识别用的 Token，只存 Vault。保存后，目录识别会向 Discogs 查询候选；候选不能单独确认发行身份。</p>' +
      '<label>Token（' + escapeText(savedMark(discogsSettings.token_saved, discogsSettings.token_revision)) + '，留空保留）<input type="password" id="discogs-token" autocomplete="off"></label>' +
      '<p><button type="button" id="save-discogs" class="primary">保存</button> <button type="button" id="test-discogs">测试连接</button></p><p id="discogs-test-result" class="meta"></p></article>';
  }
  function libraryCard() {
    if (!librarySettings) return "";
    return '<article><h2>音乐库目录</h2><p class="meta">新入库的音频放这里；改目录只影响新入库，已入库的文件不动，各自按入库时的目录解析。目录须为绝对路径，不能是磁盘根目录，不能与状态/备份目录重叠。</p>' +
      '<label>音乐库目录<input type="text" id="library-dir" value="' + escapeText(librarySettings.library_dir || "") + '" placeholder="留空用默认：' + escapeText(librarySettings.default_library_dir || "") + '"></label>' +
      '<p class="meta">当前生效：' + escapeText(librarySettings.effective_library_dir || "") + '</p>' +
      '<p><button type="button" id="save-library" class="primary">保存</button></p></article>';
  }
  function serversCard() {
    if (!serverSettings) return "";
    var plex = serverSettings.plex || {};
    var navidrome = serverSettings.navidrome || {};
    return '<article><h2>音乐服务器</h2><p class="meta">入库完成后通知服务器重新扫描：先 Navidrome，再 Plex；通知失败只记在任务说明里，不会阻断入库。Token 与密码只存 Vault，不回显。</p>' +
      '<h2>Plex</h2>' +
      '<label><input type="checkbox" id="plex-enabled"' + (plex.enabled ? " checked" : "") + '> 启用 Plex 通知</label>' +
      '<label>地址<input type="text" id="plex-url" value="' + escapeText(plex.url || "") + '" placeholder="http://127.0.0.1:32400"></label>' +
      '<label>音乐库 ID<input type="text" id="plex-section-id" value="' + escapeText(plex.section_id || "") + '" placeholder="先测试读取音乐库列表"></label>' +
      '<label>Token（' + escapeText(savedMark(plex.token_saved, plex.token_revision)) + '，留空保留）<input type="password" id="plex-token" autocomplete="off"></label>' +
      '<p><button type="button" id="save-plex" class="primary">保存 Plex</button> <button type="button" id="test-plex">测试并读取音乐库列表</button></p><p id="plex-test-result" class="meta"></p><p id="plex-sections"></p>' +
      '<h2>Navidrome</h2>' +
      '<label><input type="checkbox" id="navidrome-enabled"' + (navidrome.enabled ? " checked" : "") + '> 启用 Navidrome 通知</label>' +
      '<label>地址<input type="text" id="navidrome-url" value="' + escapeText(navidrome.url || "") + '"></label>' +
      '<label>用户名<input type="text" id="navidrome-username" value="' + escapeText(navidrome.username || "") + '" autocomplete="off"></label>' +
      '<label>密码（' + escapeText(savedMark(navidrome.password_saved, navidrome.password_revision)) + '，留空保留）<input type="password" id="navidrome-password" autocomplete="off"></label>' +
      '<p><button type="button" id="save-navidrome" class="primary">保存 Navidrome</button> <button type="button" id="test-navidrome">测试连接</button></p><p id="navidrome-test-result" class="meta"></p>' +
      '<h2>Roon</h2><p class="meta">只检查端口是否可连接，不注册、不扫描。</p><label>主机<input type="text" id="roon-host" value="127.0.0.1"></label><label>端口<input type="number" id="roon-port" value="9330"></label><p><button type="button" id="test-roon">检查 Roon</button></p><p id="roon-result" class="meta"></p></article>';
  }
  function playlistsCard() {
    var rows = playlistRows.map(function (row) {
      return '<p class="meta">' + escapeText((row.name || row.id) + "（" + row.track_count + " 首）") + (row.error ? " · " + escapeText(row.error) : "") + '</p>';
    }).join("");
    var subs = subscriptionRows.map(function (row) {
      return '<p class="meta">' + escapeText((row.name || row.playlist_id) + " · 每 " + row.interval_hours + " 小时 · " + (row.paused ? "已暂停 · " : "") + (row.last_result || "")) +
        ' <button type="button" data-sub-check="' + escapeText(row.playlist_id) + '">立即检查</button>' +
        ' <button type="button" data-sub-pause="' + escapeText(row.playlist_id) + '" data-paused="' + (row.paused ? "1" : "0") + '">' + (row.paused ? "恢复" : "暂停") + '</button>' +
        ' <button type="button" data-sub-delete="' + escapeText(row.playlist_id) + '">删除</button></p>';
    }).join("");
    return '<article><h2>歌单</h2><p class="meta">可导入 .m3u、.m3u8、.json、.csv、.tsv、.txt、.xlsx。只读取歌名、艺人和专辑列，文件里的链接不会被打开。保存后可在搜索时选用。xlsx 不执行宏。</p>' +
      '<label>歌单名称<input type="text" id="playlist-name" placeholder="留空用文件里的名称"></label>' +
      '<label>文本顺序（仅 TXT）<select id="playlist-order"><option value="artist-title">歌手 - 歌名</option><option value="title-artist">歌名 - 歌手</option></select></label>' +
      '<label>上传歌单文件<input type="file" id="playlist-file" accept=".m3u,.m3u8,.json,.csv,.tsv,.txt,.xlsx"></label>' +
      '<label>或粘贴歌单内容<textarea id="playlist-content" rows="4" placeholder="歌手 - 歌名，或带表头的 CSV / JSON"></textarea></label>' +
      '<p><button type="button" id="save-playlist" class="primary">保存歌单</button></p>' +
      (rows || '<p class="meta">还没有已存歌单。</p>') +
      '<h2>识别歌单链接</h2><p class="meta">只从链接里读出平台和歌单 ID，不访问平台，也不拉取曲目。</p>' +
      '<label>分享链接或歌单 ID<input type="text" id="playlist-link" placeholder="https://music.163.com/playlist?id=123"></label>' +
      '<p><button type="button" id="locate-playlist">识别链接</button></p><p id="playlist-locate-result" class="meta"></p>' +
      '<h2>拉取公开歌单</h2><p class="meta">按旧版只读接口读取网易云、酷狗、汽水、咪咕、Apple Music、Spotify、YouTube、QQ 音乐或酷我的曲目名单。不下载音频。YouTube 必须用用户自己的 API Key，不会改去读网页。本机测试地址只接受回环。</p>' +
      '<label>平台<select id="remote-platform"><option value="netease">网易云音乐</option><option value="kugou">酷狗</option><option value="qishui">汽水</option><option value="migu">咪咕</option><option value="applemusic">Apple Music</option><option value="spotify">Spotify</option><option value="youtube">YouTube</option><option value="qqmusic">QQ 音乐</option><option value="kuwo">酷我</option></select></label>' +
      '<label>歌单 ID<input type="text" id="remote-playlist-id" placeholder="数字 ID，或 Spotify / Apple / YouTube 的歌单 ID"></label>' +
      '<label>本机测试地址<input type="text" id="remote-base" placeholder="留空则访问各平台公开接口"></label>' +
      '<p><button type="button" id="fetch-playlist">拉取并订阅</button></p><p id="playlist-fetch-result" class="meta"></p>' +
      '<h2>订阅已存歌单</h2><p class="meta">未绑定平台时只重新读取本机歌单。绑定了公开歌单后，检查会向该平台读取新增曲目。首次默认只记下现有曲目。删除的曲目不会从音乐库移除。</p>' +
      '<label>歌单<select id="subscription-playlist"><option value="">选择已存歌单</option>' + playlistRows.map(function (row) { return '<option value="' + escapeText(row.id) + '">' + escapeText(row.name || row.id) + "</option>"; }).join("") + '</select></label>' +
      '<label>间隔（小时）<input type="number" id="subscription-hours" min="1" max="168" value="6"></label>' +
      '<label><input type="checkbox" id="subscription-existing"> 现有曲目也入库</label>' +
      '<p><button type="button" id="save-subscription" class="primary">订阅</button></p>' +
      (subs || '<p class="meta">还没有订阅。</p>') + '</article>';
  }
  function migrationCard() {
    var report = migrationReport || {};
    var counts = report.report || {};
    var summary = report.found ? ("已迁入曲目 " + (counts.library || 0) + "，待核对任务 " + (counts.jobs || 0) + "，令牌记录 " + (counts.tokens || 0) + "。") : "还没有旧版活动数据回执。";
    return '<article><h2>从旧版迁入</h2><p class="meta">' + escapeText(report.note || "启动时会读取旧版数据库。音乐文件不移动。") + "</p><p>" + escapeText(summary) + "</p>" +
      '<p class="meta">冷归档' + (report.cold_archive ? "仍在" : "还没有") + "。自动安装更新：" + (report.auto_install ? "会" : "不会") + "。</p>" +
      '<p><button type="button" id="rollback-migration">回退迁入的索引</button></p><p id="migration-result" class="meta"></p></article>';
  }
  function renderSettings() {
    if (!downloadSettings && !cookieSettings.length && !aiSettings && !qbtSettings && !solverSettings && !lyricsSettings && !discogsSettings && !librarySettings && !serverSettings) {
      setStatus("", "设置没有读到。");
      list.innerHTML = "";
      return;
    }
    setStatus("", "");
    var html = '<div class="cards">' + migrationCard() + downloadCard() + cookieCard() + aiCard() + qbtCard() + solverCard() + lyricsCard() + discogsCard() + libraryCard() + serversCard() + playlistsCard() +
      '<article><h2>站点登录检查</h2><p class="meta">只读检查：只验证登录与余额，不下载、不花 token。Cookie 留空用已保存的，填了则只用于这次检查，不保存。</p>' +
      '<label>站点<select id="preflight-site">' + siteRows.map(function (site) { return '<option value="' + escapeText(site.name) + '">' + escapeText(site.name) + "</option>"; }).join("") + '</select></label>' +
      '<label>Cookie<input type="password" id="preflight-cookie" autocomplete="off"></label>' +
      '<label><input type="checkbox" id="preflight-synthetic"> 使用测试服务器（不访问真实站点）</label>' +
      '<label>测试服务器地址<input type="text" id="preflight-base-url"></label>' +
      '<p><button type="button" id="run-preflight" class="primary">检查登录</button></p><p id="preflight-result" class="meta"></p></article></div>';
    list.innerHTML = html;
    var save = document.getElementById("save-download");
    if (save) save.addEventListener("click", saveDownload);
    document.querySelectorAll("[data-order-move]").forEach(function (button) {
      button.addEventListener("click", function () {
        var parts = button.getAttribute("data-order-move").split(":");
        moveOrder(parseInt(parts[0], 10), parseInt(parts[1], 10));
      });
    });
    document.querySelectorAll("[data-cookie-site]").forEach(function (button) {
      button.addEventListener("click", function () { saveCookie(button.getAttribute("data-cookie-site")); });
    });
    var saveAIButton = document.getElementById("save-ai");
    if (saveAIButton) saveAIButton.addEventListener("click", saveAI);
    var testAIButton = document.getElementById("test-ai");
    if (testAIButton) testAIButton.addEventListener("click", testAI);
    var saveQBTButton = document.getElementById("save-qbt");
    if (saveQBTButton) saveQBTButton.addEventListener("click", saveQBT);
    var testQBTButton = document.getElementById("test-qbt");
    if (testQBTButton) testQBTButton.addEventListener("click", testQBT);
    var saveSolverButton = document.getElementById("save-solver");
    if (saveSolverButton) saveSolverButton.addEventListener("click", saveSolver);
    var testSolverButton = document.getElementById("test-solver");
    if (testSolverButton) testSolverButton.addEventListener("click", testSolver);
    var saveLyricsButton = document.getElementById("save-lyrics");
    if (saveLyricsButton) saveLyricsButton.addEventListener("click", saveLyrics);
    var testLyricsButton = document.getElementById("test-lyrics");
    if (testLyricsButton) testLyricsButton.addEventListener("click", testLyrics);
    var saveDiscogsButton = document.getElementById("save-discogs");
    if (saveDiscogsButton) saveDiscogsButton.addEventListener("click", saveDiscogs);
    var testDiscogsButton = document.getElementById("test-discogs");
    if (testDiscogsButton) testDiscogsButton.addEventListener("click", testDiscogs);
    var saveLibraryButton = document.getElementById("save-library");
    if (saveLibraryButton) saveLibraryButton.addEventListener("click", saveLibrary);
    var rollbackMigrationButton = document.getElementById("rollback-migration");
    if (rollbackMigrationButton) rollbackMigrationButton.addEventListener("click", rollbackMigration);
    var savePlexButton = document.getElementById("save-plex");
    if (savePlexButton) savePlexButton.addEventListener("click", savePlex);
    var testPlexButton = document.getElementById("test-plex");
    if (testPlexButton) testPlexButton.addEventListener("click", testPlex);
    var saveNavidromeButton = document.getElementById("save-navidrome");
    if (saveNavidromeButton) saveNavidromeButton.addEventListener("click", saveNavidrome);
    var testNavidromeButton = document.getElementById("test-navidrome");
    if (testNavidromeButton) testNavidromeButton.addEventListener("click", testNavidrome);
    var unifyQbt = document.getElementById("unify-qbt-tags");
    if (unifyQbt) unifyQbt.addEventListener("click", unifyQbtTags);
    var testRoonButton = document.getElementById("test-roon");
    if (testRoonButton) testRoonButton.addEventListener("click", testRoon);
    var savePlaylistButton = document.getElementById("save-playlist");
    if (savePlaylistButton) savePlaylistButton.addEventListener("click", savePlaylist);
    var locatePlaylistButton = document.getElementById("locate-playlist");
    if (locatePlaylistButton) locatePlaylistButton.addEventListener("click", locatePlaylist);
    var fetchPlaylistButton = document.getElementById("fetch-playlist");
    if (fetchPlaylistButton) fetchPlaylistButton.addEventListener("click", fetchRemotePlaylist);
    var saveSubscriptionButton = document.getElementById("save-subscription");
    if (saveSubscriptionButton) saveSubscriptionButton.addEventListener("click", saveSubscription);
    document.querySelectorAll("[data-sub-check]").forEach(function (button) {
      button.addEventListener("click", function () { checkSubscription(button.getAttribute("data-sub-check")); });
    });
    document.querySelectorAll("[data-sub-pause]").forEach(function (button) {
      button.addEventListener("click", function () { pauseSubscription(button.getAttribute("data-sub-pause"), button.getAttribute("data-paused") !== "1"); });
    });
    document.querySelectorAll("[data-sub-delete]").forEach(function (button) {
      button.addEventListener("click", function () { deleteSubscription(button.getAttribute("data-sub-delete")); });
    });
    var run = document.getElementById("run-preflight");
    if (run) run.addEventListener("click", preflight);
  }
  async function saveDownload() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var engine = document.getElementById("download-engine");
      var body = await postSetting("/v1/settings/download", {streaming_enabled: document.getElementById("download-streaming").checked, streaming_order: streamingOrder, engine_url: engine ? engine.value.trim() : ""});
      downloadSettings.streaming_enabled = body.streaming_enabled;
      downloadSettings.engine_url = body.engine_url || "";
      if (body.streaming_order && body.streaming_order.length) streamingOrder = body.streaming_order.slice();
      setStatus("ok", "已保存。");
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveCookie(site) {
    if (loading) return;
    var field = document.getElementById("cookie-" + site);
    setBusy(true);
    setStatus("info", "正在保存 " + site + " Cookie");
    try {
      var body = await postSetting("/v1/settings/cookies", {site: site, cookie_json: field ? field.value : ""});
      cookieSettings = Array.isArray(body.sites) ? body.sites : [];
      setStatus("ok", site + " Cookie 已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveAI() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/ai", {
        enabled: document.getElementById("ai-enabled").checked,
        base_url: document.getElementById("ai-base-url").value.trim(),
        model: document.getElementById("ai-model").value.trim(),
        api_key: document.getElementById("ai-key").value,
        timeout: parseInt(document.getElementById("ai-timeout").value, 10) || undefined,
        max_output_tokens: parseInt(document.getElementById("ai-max-tokens").value, 10) || undefined,
        temperature: parseFloat(document.getElementById("ai-temperature").value)
      });
      aiSettings = body.ai;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testAI() {
    if (loading) return;
    var result = document.getElementById("ai-test-result");
    setBusy(true);
    setStatus("info", "正在测试 AI 连接");
    try {
      var body = await postSetting("/v1/settings/ai/test", {});
      if (result) result.textContent = body.message || "AI 连接成功。";
      setStatus("ok", body.message || "AI 连接成功。");
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function unifyQbtTags() {
    var result = document.getElementById("qbt-test-result");
    try {
      var body = await postSetting("/v1/settings/qbittorrent/tags", {
        base_url: document.getElementById("qbt-base-url").value.trim(),
        username: document.getElementById("qbt-username").value.trim(),
        password: document.getElementById("qbt-password").value
      });
      if (result) result.textContent = body.note || "已统一旧标签。";
    } catch (err) {
      if (result) result.textContent = err.message;
    }
  }
  async function testRoon() {
    var result = document.getElementById("roon-result");
    try {
      var body = await postSetting("/v1/settings/servers/roon/test", {
        host: document.getElementById("roon-host").value.trim(),
        port: parseInt(document.getElementById("roon-port").value, 10) || 9330
      });
      if (result) result.textContent = body.note || "";
    } catch (err) {
      if (result) result.textContent = err.message;
    }
  }
  async function saveQBT() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/qbittorrent", {
        base_url: document.getElementById("qbt-base-url").value.trim(),
        username: document.getElementById("qbt-username").value.trim(),
        password: document.getElementById("qbt-password").value,
        category: document.getElementById("qbt-category").value.trim(),
        tags: document.getElementById("qbt-tags").value.trim()
      });
      qbtSettings = body.qbittorrent;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testQBT() {
    if (loading) return;
    var resultBox = document.getElementById("qbt-test-result");
    setBusy(true);
    setStatus("info", "正在测试 qBittorrent 连接");
    try {
      var body = await postSetting("/v1/settings/qbittorrent/test", {});
      var info = body.qbittorrent || {};
      var text = body.message || "qBittorrent 连接成功。";
      if (info.version) text += "版本 " + info.version + "，";
      if (info.torrent_count != null) text += "种子 " + info.torrent_count + " 个。";
      if (resultBox) resultBox.textContent = text;
      setStatus("ok", text);
    } catch (err) {
      if (resultBox) resultBox.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveSolver() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/solver", {
        managed: document.getElementById("solver-managed").checked,
        enabled: document.getElementById("solver-enabled").checked,
        base_url: document.getElementById("solver-base-url").value.trim(),
        timeout: parseInt(document.getElementById("solver-timeout").value, 10) || undefined,
        executable: document.getElementById("solver-executable").value.trim(),
        auto_start: document.getElementById("solver-auto-start").checked
      });
      solverSettings = body.solver;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testSolver() {
    if (loading) return;
    var result = document.getElementById("solver-test-result");
    setBusy(true);
    setStatus("info", "正在测试 FlareSolverr 连接");
    try {
      var body = await postSetting("/v1/settings/solver/test", {});
      var text = body.message || "FlareSolverr 连接成功。";
      if (body.sessions != null) text += "会话 " + body.sessions + " 个。";
      if (body.version) text += "版本 " + body.version + "。";
      if (result) result.textContent = text;
      setStatus("ok", text);
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveLyrics() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/lyrics", {
        base_url: document.getElementById("lyrics-base-url").value.trim(),
        lyrics_path: document.getElementById("lyrics-path").value.trim(),
        candidates_path: document.getElementById("lyrics-candidates-path").value.trim(),
        cover_path: document.getElementById("lyrics-cover-path").value.trim(),
        write_mode: document.getElementById("lyrics-write-mode").value,
        auth_header: document.getElementById("lyrics-auth-header").value.trim(),
        authorization: document.getElementById("lyrics-auth").value
      });
      lyricsSettings = body.lyrics;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testLyrics() {
    if (loading) return;
    var result = document.getElementById("lyrics-test-result");
    setBusy(true);
    setStatus("info", "正在测试歌词服务连接");
    try {
      var body = await postSetting("/v1/settings/lyrics/test", {});
      var text = body.message || "歌词服务连接成功。";
      if (body.valid != null) text += "可用候选 " + body.valid + " 条。";
      if (result) result.textContent = text;
      setStatus("ok", text);
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveDiscogs() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/discogs", {token: document.getElementById("discogs-token").value});
      discogsSettings = body.discogs;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testDiscogs() {
    if (loading) return;
    var result = document.getElementById("discogs-test-result");
    setBusy(true);
    setStatus("info", "正在测试 Discogs 连接");
    try {
      var body = await postSetting("/v1/settings/discogs/test", {});
      if (result) result.textContent = body.message || "Discogs Token 可用。";
      setStatus("ok", body.message || "Discogs Token 可用。");
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveLibrary() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/library", {library_dir: document.getElementById("library-dir").value.trim()});
      librarySettings = body.library;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function savePlex() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/servers/plex", {
        enabled: document.getElementById("plex-enabled").checked,
        url: document.getElementById("plex-url").value.trim(),
        section_id: document.getElementById("plex-section-id").value.trim(),
        token: document.getElementById("plex-token").value
      });
      serverSettings = body.servers;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testPlex() {
    if (loading) return;
    var result = document.getElementById("plex-test-result");
    var sectionsBox = document.getElementById("plex-sections");
    setBusy(true);
    setStatus("info", "正在测试 Plex 连接");
    try {
      var body = await postSetting("/v1/settings/servers/plex/test", {});
      var text = body.message || "Plex 连接成功。";
      if (result) result.textContent = text + "共 " + (body.count || 0) + " 个音乐库。";
      if (sectionsBox) {
        sectionsBox.innerHTML = (body.sections || []).map(function (section) {
          return '<button type="button" data-plex-section="' + escapeText(section.id) + '">' + escapeText(section.name + "（" + section.id + "）") + "</button> ";
        }).join("");
        sectionsBox.querySelectorAll("[data-plex-section]").forEach(function (button) {
          button.addEventListener("click", function () {
            document.getElementById("plex-section-id").value = button.getAttribute("data-plex-section");
          });
        });
      }
      setStatus("ok", text);
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveNavidrome() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var body = await postSetting("/v1/settings/servers/navidrome", {
        enabled: document.getElementById("navidrome-enabled").checked,
        url: document.getElementById("navidrome-url").value.trim(),
        username: document.getElementById("navidrome-username").value.trim(),
        password: document.getElementById("navidrome-password").value
      });
      serverSettings = body.servers;
      setStatus("ok", "已保存。");
      renderSettings();
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testNavidrome() {
    if (loading) return;
    var result = document.getElementById("navidrome-test-result");
    setBusy(true);
    setStatus("info", "正在测试 Navidrome 连接");
    try {
      var body = await postSetting("/v1/settings/servers/navidrome/test", {});
      if (result) result.textContent = body.message || "Navidrome 连接成功。";
      setStatus("ok", body.message || "Navidrome 连接成功。");
    } catch (err) {
      if (result) result.textContent = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      setStatus("fail", "测试失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function savePlaylist() {
    if (loading) return;
    var fileInput = document.getElementById("playlist-file");
    var name = document.getElementById("playlist-name").value.trim();
    var content = document.getElementById("playlist-content").value;
    setBusy(true);
    setStatus("info", "正在保存歌单");
    try {
      var response;
      if (fileInput && fileInput.files && fileInput.files.length) {
        var form = new FormData();
        form.append("file", fileInput.files[0]);
        if (name) form.append("name", name);
        form.append("text_order", document.getElementById("playlist-order").value);
        response = await fetch("/v1/playlists", {method: "POST", body: form});
      } else if (content.trim()) {
        response = await fetch("/v1/playlists", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({content: content, name: name, text_order: document.getElementById("playlist-order").value})});
      } else {
        setStatus("fail", "先上传歌单文件或粘贴歌单内容。");
        return;
      }
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "保存失败。" + (body && (body.error || body.code) ? "（" + (body.error || body.code) + "）" : ""));
        return;
      }
      setStatus("ok", "歌单已保存：" + (body.playlist.name || body.playlist.id) + "，" + body.playlist.track_count + " 首。");
      load();
    } catch (err) {
      setStatus("fail", "保存中断。");
    } finally {
      setBusy(false);
    }
  }
  async function locatePlaylist() {
    if (loading) return;
    var text = document.getElementById("playlist-link").value.trim();
    if (!text) { setStatus("fail", "先粘贴歌单链接或 ID。"); return; }
    setBusy(true);
    setStatus("info", "正在识别链接");
    try {
      var body = await postSetting("/v1/playlists/locate", {text: text, platform: "auto"});
      var lines = (body.playlists || []).map(function (row) {
        return (row.name || row.platform) + (row.playlist_id ? " · " + row.playlist_id : "") + " · " + (row.note || "");
      });
      var result = document.getElementById("playlist-locate-result");
      if (result) result.textContent = lines.join("；") || body.note || "";
      setStatus("ok", body.note || "已识别。");
    } catch (err) {
      setStatus("fail", "识别失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function fetchRemotePlaylist() {
    if (loading) return;
    var id = document.getElementById("remote-playlist-id").value.trim();
    if (!id) { setStatus("fail", "先填写歌单 ID。"); return; }
    setBusy(true);
    setStatus("info", "正在读取公开歌单");
    try {
      var payload = {platform: document.getElementById("remote-platform").value, playlist_id: id, save: true, subscribe: true};
      var base = document.getElementById("remote-base");
      if (base && base.value.trim()) payload.base_url = base.value.trim();
      var body = await postSetting("/v1/playlists/fetch", payload);
      var result = document.getElementById("playlist-fetch-result");
      if (result) result.textContent = body.note || "";
      setStatus("ok", body.note || "已读取曲目名单。");
      load();
    } catch (err) {
      setStatus("fail", "没有读到歌单。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function saveSubscription() {
    if (loading) return;
    var playlistID = document.getElementById("subscription-playlist").value;
    if (!playlistID) { setStatus("fail", "先选择已存歌单。"); return; }
    setBusy(true);
    try {
      await postSetting("/v1/subscriptions", {playlist_id: playlistID, interval_hours: parseInt(document.getElementById("subscription-hours").value, 10) || 6, include_existing: document.getElementById("subscription-existing").checked});
      setStatus("ok", "已订阅。首次检查前不会创建任务。");
      load();
    } catch (err) {
      setStatus("fail", "订阅失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function checkSubscription(playlistID) {
    if (loading || !playlistID) return;
    setBusy(true);
    setStatus("info", "正在检查订阅");
    try {
      var body = await postSetting("/v1/subscriptions/check", {playlist_id: playlistID});
      setStatus("ok", body.message || "检查完成。");
      load();
    } catch (err) {
      setStatus("fail", "检查失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function pauseSubscription(playlistID, paused) {
    if (loading || !playlistID) return;
    setBusy(true);
    try {
      await postSetting("/v1/subscriptions/pause", {playlist_id: playlistID, paused: paused});
      setStatus("ok", paused ? "已暂停订阅。" : "已恢复订阅。");
      load();
    } catch (err) {
      setStatus("fail", "操作失败。");
    } finally {
      setBusy(false);
    }
  }
  async function deleteSubscription(playlistID) {
    if (loading || !playlistID) return;
    setBusy(true);
    try {
      await postSetting("/v1/subscriptions/delete", {playlist_id: playlistID});
      setStatus("ok", "已删除订阅。");
      load();
    } catch (err) {
      setStatus("fail", "删除失败。");
    } finally {
      setBusy(false);
    }
  }
  async function preflight() {
    if (loading) return;
    var cookie = document.getElementById("preflight-cookie").value;
    var synthetic = document.getElementById("preflight-synthetic").checked;
    var baseURL = document.getElementById("preflight-base-url").value.trim();
    if (synthetic && !baseURL) { setStatus("fail", "使用测试服务器时需要填写地址。"); return; }
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
    var duplicateHtml = "";
    if (duplicateRows.length) {
      duplicateRows.forEach(function (group) {
        duplicateHtml += '<p>' + escapeText((group.artists || []).join("、") + " / " + (group.album || "")) + " · " + escapeText(group.state || "") + " · " + escapeText(group.reason || "") +
          (group.state === "ready" ? ' <button type="button" data-merge="' + escapeText(group.group_id) + '">合并重复项</button>' : "") + "</p>";
      });
    } else {
      duplicateHtml = '<p class="meta">尚未扫描，或没有可合并的重复项。</p>';
    }
    var html = '<div class="cards"><article><h2>本地资料库</h2><p class="meta">重新索引只补充新文件，不删除磁盘上的音乐。先逐页勾选曲目，确认后才进入检索。下载仍受令牌规则约束，这一步不会扣费，也不会入库。</p>' +
      '<p><button type="button" id="reindex-library" class="primary">重新索引</button> <button type="button" id="list-gaps">列出缺失曲目</button></p>' +
      '<p><button type="button" id="library-page-prev">上一页</button> <button type="button" id="library-page-next">下一页</button> <span id="library-page-label" class="meta"></span></p>' +
      '<div id="library-file"></div>' +
      '<p><button type="button" id="fill-gaps">确认所选并补缺</button> <button type="button" id="upgrade-album">确认所选并升级</button> <button type="button" id="expand-album">确认所选并扩成整专</button></p>' +
      '<p id="library-ops-result" class="meta"></p></article>' +
      '<article><h2>标签、封面和歌词</h2><p class="meta">目录确认后，标签、歌词和封面同一次写入。封面写入 FLAC 内嵌图片；按设置可以同时保留同名 jpg。写之前会备份，任何一步失败都恢复原文件。目录、歌词和封面地址只接受本机。</p>' +
      '<label>文件路径<input type="text" id="meta-path" placeholder="库内 .flac"></label>' +
      '<label>艺人<input type="text" id="meta-artist"></label><label>专辑<input type="text" id="meta-album"></label><label>曲名<input type="text" id="meta-title"></label>' +
      '<label>目录地址<input type="text" id="meta-catalog" placeholder="本机 MusicBrainz"></label>' +
      '<label>歌词地址<input type="text" id="meta-lyrics" placeholder="留空则不写歌词"></label>' +
      '<label>封面地址<input type="text" id="meta-cover" placeholder="留空则不写封面"></label>' +
      '<label>封面写入<select id="meta-cover-mode"><option value="sidecar">内嵌并保留同名 jpg</option><option value="embedded">只写入 FLAC 内嵌图片</option></select></label>' +
      '<p><button type="button" id="complete-metadata">补齐标签封面歌词</button></p><p id="metadata-result" class="meta"></p></article>' +
      '<article><h2>合辑</h2><p class="meta">预览不会移动文件。合并只把名称里带“合辑”或 Various Artists 的文件夹移进音乐库的 Compilations。</p>' +
      '<p><button type="button" id="preview-compilations">预览合辑</button> <button type="button" id="merge-compilations">合并合辑</button></p><p id="compilation-result" class="meta"></p></article>' +
      '<article><h2>艺人目录与 NCM</h2><p class="meta">艺人目录把专辑文件夹整理成“艺人/专辑”。NCM 解密不改源文件。</p>' +
      '<p><button type="button" id="plan-artist-folders">查看艺人目录</button> <button type="button" id="apply-artist-folders">整理艺人目录</button></p>' +
      '<label>NCM 路径<input type="text" id="ncm-source" placeholder="库内 .ncm 路径"></label><p><button type="button" id="decrypt-ncm">解密 NCM</button></p><p id="library-tools-result" class="meta"></p></article>' +
      '<article><h2>重复歌曲</h2><p class="meta">同名不等于重复。内容一致且可以确定保留哪一份时才能合并，其余进入核对。合并后的旧文件进隔离区，可以恢复。</p>' +
      '<p><button type="button" id="scan-duplicates" class="primary">扫描重复</button></p>' + duplicateHtml +
      '<label>恢复编号<input type="text" id="restore-merge-id" placeholder="合并后返回的编号"></label><p><button type="button" id="restore-duplicates">恢复这次合并</button></p></article>' +
      '<article><form id="transcode-form"><h2>WAV 无损转 FLAC</h2>' +
      '<p class="meta">整数 PCM（16/24/32 位、多声道、可扩展格式）用纯 Go 转成 FLAC，样本逐个比对并写入 MD5。浮点和压缩 WAV 会拒绝，不会降级。标签从 LIST/INFO、ID3 和 BWF 迁入，封面写入内嵌图片。没有标签时只按文件名和目录补显示名，不把它当成已确认的发行。</p>' +
      '<label><input type="checkbox" id="wav-auto"> 下载进入待入库或重新索引时，先转成 FLAC 再识别入库</label>' +
      '<label>原 WAV<select id="wav-disposal"><option value="keep">保留原文件</option><option value="recycle">移入可恢复回收目录</option><option value="delete">校验通过后删除</option></select></label>' +
      '<p><button type="button" id="save-wav" class="primary">保存转换设置</button></p><p id="wav-setting-note" class="meta"></p>' +
      '<label>单个 WAV<input type="text" id="transcode-source" placeholder="库内或待入库 .wav"></label>' +
      '<label>批量目录<input type="text" id="transcode-dir" placeholder="留空则只转上面一个文件"></label>' +
      '<label>多个文件，一行一个<textarea id="transcode-paths" rows="3" placeholder="可选"></textarea></label>' +
      '<p><button type="submit" class="primary">开始批量转换</button> <button type="button" id="transcode-pause">暂停</button> <button type="button" id="transcode-cancel">取消</button></p>' +
      '<p id="transcode-result" class="meta"></p></form></article>';
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
      bindLibrary();
      return;
    }
    setStatus("", "");
    libraryRows.forEach(function (entry) {
      html += '<article><h2>' + escapeText(entry.file || entry.source_name || "未命名") + '</h2><p class="meta"><span class="pill">' + escapeText(entry.format || "") + '</span><span class="pill">' + escapeText(entry.decode_state || "") + '</span><span class="pill">' + escapeText(entry.origin || "") + '</span></p><p>' + (entry.file_present ? "文件在库中" : "文件不在库中") + " · " + escapeText((entry.sha256 || "").slice(0, 12)) + "</p></article>";
    });
    list.innerHTML = html + "</div>";
    bindLibrary();
  }
  function bindLibrary() {
    var form = document.getElementById("transcode-form");
    if (form) form.addEventListener("submit", submitTranscode);
    var saveWav = document.getElementById("save-wav");
    if (saveWav) saveWav.addEventListener("click", saveWAVSettings);
    var pauseWav = document.getElementById("transcode-pause");
    if (pauseWav) pauseWav.addEventListener("click", function () { controlTranscode("pause"); });
    var cancelWav = document.getElementById("transcode-cancel");
    if (cancelWav) cancelWav.addEventListener("click", function () { controlTranscode("cancel"); });
    loadWAVSettings();
    var reindex = document.getElementById("reindex-library");
    if (reindex) reindex.addEventListener("click", function () { libraryAction("/v1/library/reindex", {}, "library-ops-result"); });
    var gaps = document.getElementById("list-gaps");
    if (gaps) gaps.addEventListener("click", function () { libraryAction("/v1/library/gaps", null, "library-ops-result"); });
    var fill = document.getElementById("fill-gaps");
    if (fill) fill.addEventListener("click", function () { confirmTracks("fill", "补缺"); });
    var upgrade = document.getElementById("upgrade-album");
    if (upgrade) upgrade.addEventListener("click", function () { confirmTracks("upgrade", "音质升级"); });
    var expand = document.getElementById("expand-album");
    if (expand) expand.addEventListener("click", function () { confirmTracks("expand", "单曲升级整专"); });
    var pagePrev = document.getElementById("library-page-prev");
    if (pagePrev) pagePrev.addEventListener("click", function () { libraryFileOffset = Math.max(0, libraryFileOffset - 20); loadLibraryFiles(); });
    var pageNext = document.getElementById("library-page-next");
    if (pageNext) pageNext.addEventListener("click", function () { libraryFileOffset += 20; loadLibraryFiles(); });
    var metadata = document.getElementById("complete-metadata");
    if (metadata) metadata.addEventListener("click", completeMetadata);
    loadLibraryFiles();
    var previewCompilations = document.getElementById("preview-compilations");
    if (previewCompilations) previewCompilations.addEventListener("click", function () { libraryAction("/v1/library/compilations", null, "compilation-result"); });
    var mergeCompilations = document.getElementById("merge-compilations");
    if (mergeCompilations) mergeCompilations.addEventListener("click", function () { libraryAction("/v1/library/compilations/merge", {}, "compilation-result"); });
    var planArtists = document.getElementById("plan-artist-folders");
    if (planArtists) planArtists.addEventListener("click", function () { libraryAction("/v1/library/artist-folders", null, "library-tools-result"); });
    var applyArtists = document.getElementById("apply-artist-folders");
    if (applyArtists) applyArtists.addEventListener("click", function () { libraryAction("/v1/library/artist-folders", {}, "library-tools-result"); });
    var decrypt = document.getElementById("decrypt-ncm");
    if (decrypt) decrypt.addEventListener("click", function () { libraryAction("/v1/library/ncm/decrypt", {source: document.getElementById("ncm-source").value.trim()}, "library-tools-result"); });
    var scan = document.getElementById("scan-duplicates");
    if (scan) scan.addEventListener("click", scanDuplicates);
    var restore = document.getElementById("restore-duplicates");
    if (restore) restore.addEventListener("click", restoreDuplicates);
    document.querySelectorAll("[data-merge]").forEach(function (button) {
      button.addEventListener("click", function () { mergeDuplicates(button.getAttribute("data-merge")); });
    });
  }
  function confirmTracks(scene, label) {
    var paths = Object.keys(selectedTracks).filter(function (path) { return selectedTracks[path]; });
    var result = document.getElementById("library-ops-result");
    if (!paths.length) {
      if (result) result.textContent = "请先逐首勾选，再进入下载。这一步尚未下载、尚未入库。";
      return;
    }
    libraryAction("/v1/library/workflow", {scene: scene, label: label, paths: paths, confirmed: true}, "library-ops-result");
  }
  async function loadLibraryFiles() {
    var list = document.getElementById("library-file");
    var label = document.getElementById("library-page-label");
    if (!list) return;
    try {
      var response = await fetch("/v1/library/files?offset=" + libraryFileOffset + "&limit=20", {headers: {Accept: "application/json"}});
      var body = await response.json();
      var total = body.total || 0;
      if (libraryFileOffset > 0 && libraryFileOffset >= total) {
        libraryFileOffset = Math.max(0, total - 20);
      }
      var html = "";
      (body.files || []).forEach(function (file) {
        html += '<label><input type="checkbox" class="library-track" value="' + escapeText(file) + '"' + (selectedTracks[file] ? " checked" : "") + "> " + escapeText(file) + "</label>";
      });
      list.innerHTML = html || '<p class="meta">这一页没有音频文件。</p>';
      if (label) label.textContent = "第 " + (libraryFileOffset + 1) + "–" + (libraryFileOffset + (body.files || []).length) + " 首，共 " + total + " 首";
      list.querySelectorAll(".library-track").forEach(function (box) {
        box.addEventListener("change", function () { selectedTracks[box.value] = box.checked; });
      });
    } catch (err) {
      list.innerHTML = '<p class="meta">没有读到库内文件。</p>';
    }
  }
  async function completeMetadata() {
    libraryAction("/v1/library/metadata", {
      path: document.getElementById("meta-path").value.trim(),
      artist: document.getElementById("meta-artist").value.trim(),
      album: document.getElementById("meta-album").value.trim(),
      title: document.getElementById("meta-title").value.trim(),
      catalog_base_url: document.getElementById("meta-catalog").value.trim(),
      lyrics_base_url: document.getElementById("meta-lyrics").value.trim(),
      cover_base_url: document.getElementById("meta-cover").value.trim(),
      cover_mode: document.getElementById("meta-cover-mode").value
    }, "metadata-result");
  }
  async function libraryAction(path, body, targetID) {
    var target = document.getElementById(targetID);
    try {
      var response = await fetch(path, {method: body === null ? "GET" : "POST", headers: {"Content-Type": "application/json", Accept: "application/json"}, body: body === null ? undefined : JSON.stringify(body)});
      var decoded = await response.json();
      if (!response.ok || decoded.ok === false) throw new Error(decoded.error || decoded.code || "没有完成");
      if (target) target.textContent = decoded.note || decoded.error || "已完成。";
      load(true);
    } catch (err) {
      if (target) target.textContent = err.message;
    }
  }
  async function scanDuplicates() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在扫描重复");
    try {
      var response = await fetch("/v1/library/duplicates", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "扫描失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      duplicateRows = body.groups || [];
      setStatus("ok", "扫描完成，" + duplicateRows.length + " 组。同名未确认的不会合并。");
      renderLibrary();
    } catch (err) {
      setStatus("fail", "扫描中断。");
    } finally {
      setBusy(false);
    }
  }
  async function mergeDuplicates(groupID) {
    if (loading || !groupID) return;
    setBusy(true);
    setStatus("info", "正在合并");
    try {
      var body = await postSetting("/v1/library/duplicates/merge", {group_id: groupID});
      var box = document.getElementById("restore-merge-id");
      if (box && body.merge_id) box.value = body.merge_id;
      setStatus("ok", "已合并，旧副本在隔离区。恢复编号 " + (body.merge_id || "") + "。");
      duplicateRows = [];
    } catch (err) {
      setStatus("fail", "合并失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function restoreDuplicates() {
    if (loading) return;
    var mergeID = document.getElementById("restore-merge-id").value.trim();
    if (!mergeID) { setStatus("fail", "先填写恢复编号。"); return; }
    setBusy(true);
    try {
      await postSetting("/v1/library/duplicates/restore", {merge_id: mergeID});
      setStatus("ok", "已从隔离区恢复。");
    } catch (err) {
      setStatus("fail", "恢复失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function loadWAVSettings() {
    var box = document.getElementById("wav-auto");
    if (!box) return;
    try {
      var response = await fetch("/v1/settings/wav", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!body || !body.wav) return;
      box.checked = !!body.wav.auto_convert_wav;
      var disposal = document.getElementById("wav-disposal");
      if (disposal && body.wav.wav_disposal) disposal.value = body.wav.wav_disposal;
      var note = document.getElementById("wav-setting-note");
      if (note) note.textContent = body.note || "";
    } catch (err) {}
  }
  async function saveWAVSettings() {
    var note = document.getElementById("wav-setting-note");
    try {
      var body = await postSetting("/v1/settings/wav", {auto_convert_wav: document.getElementById("wav-auto").checked, wav_disposal: document.getElementById("wav-disposal").value});
      if (note) note.textContent = (body && body.note) || "已保存";
    } catch (err) {
      if (note) note.textContent = err.message;
    }
  }
  async function controlTranscode(action) {
    var result = document.getElementById("transcode-result");
    if (!window.musicopsTranscodeBatch) {
      if (result) result.textContent = "还没有一批正在转换的文件。";
      return;
    }
    try {
      var response = await fetch("/v1/library/transcode/batch/control", {method: "POST", headers: {"Content-Type": "application/json", Accept: "application/json"}, body: JSON.stringify({batch_id: window.musicopsTranscodeBatch, action: action})});
      var body = await response.json();
      if (result) result.textContent = body.note || body.error || action;
    } catch (err) {
      if (result) result.textContent = err.message;
    }
  }
  async function submitTranscode(event) {
    event.preventDefault();
    if (loading) return;
    var source = document.getElementById("transcode-source");
    var dir = document.getElementById("transcode-dir");
    var extra = document.getElementById("transcode-paths");
    var paths = [];
    if (extra && extra.value.trim()) extra.value.split(/\n/).forEach(function (line) { if (line.trim()) paths.push(line.trim()); });
    if (source && source.value.trim()) paths.push(source.value.trim());
    if (!paths.length && !(dir && dir.value.trim())) { setStatus("fail", "先选择 WAV 文件或目录。"); return; }
    setBusy(true);
    setStatus("info", "正在提交转换");
    try {
      var response = await fetch("/v1/library/transcode/batch", {method: "POST", headers: {"Content-Type": "application/json", Accept: "application/json"}, body: JSON.stringify({paths: paths, dir: dir ? dir.value.trim() : "", wav_disposal: document.getElementById("wav-disposal").value})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", "转换没有提交。" + (body && (body.error || body.code) ? "（" + (body.error || body.code) + "）" : ""));
        return;
      }
      window.musicopsTranscodeBatch = body.batch_id;
      window.musicopsTranscodeNote = body.note || "已提交";
      setStatus("ok", "已提交 " + (body.count || 0) + " 个文件");
    } catch (err) {
      setStatus("fail", "转换提交中断。");
    } finally {
      setBusy(false);
    }
    await load(true);
    var result = document.getElementById("transcode-result");
    if (result && window.musicopsTranscodeNote) result.textContent = window.musicopsTranscodeNote;
  }
  function renderLogs() {
    if (!logRows.length && !logEvents.length) {
      list.innerHTML = "";
      setStatus("", "还没有运行记录。");
      return;
    }
    setStatus("", "");
    var html = '<div class="cards">';
    logEvents.forEach(function (event) {
      html += '<article><h2>' + escapeText(event.phase || "旧事件") + '</h2><p class="meta"><span class="pill">' + escapeText(event.at || "") + "</span><span class=\"pill\">" + escapeText(event.task_id || "") + "</span></p><p>" + escapeText(event.message || "") + "</p></article>";
    });
    logRows.forEach(function (task) {
      html += '<article><h2>' + escapeText(task.task_id || "") + '</h2><p class="meta"><span class="pill">' + escapeText(task.state || "") + '</span><span class="pill">' + escapeText(task.command || "") + "</span></p><p>" + escapeText(task.message || "") + "</p></article>";
    });
    list.innerHTML = html + "</div>";
  }
  async function rollbackMigration() {
    var target = document.getElementById("migration-result");
    if (!window.confirm("只撤掉迁入的曲目索引、待核对任务和令牌记录。音乐文件和冷归档会保留。")) return;
    try {
      var body = await postSetting("/v1/migration/rollback", {});
      if (target) target.textContent = body.audio_unchanged ? "已回退索引，音乐文件未改动。" : "已回退。";
      load(true);
    } catch (err) {
      if (target) target.textContent = err.message;
    }
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
      if (view === "search") {
        var playlistResponse = await fetch("/v1/playlists", {headers: {Accept: "application/json"}});
        var playlistBody = await playlistResponse.json();
        playlistRows = playlistBody && Array.isArray(playlistBody.playlists) ? playlistBody.playlists : [];
      }
      if (view === "settings") {
        var downloadResponse = await fetch("/v1/settings/download", {headers: {Accept: "application/json"}});
        var downloadBody = await downloadResponse.json();
        downloadSettings = downloadBody && downloadBody.ok ? downloadBody : null;
        if (downloadSettings) {
          streamingOrder = (downloadSettings.streaming_order && downloadSettings.streaming_order.length) ? downloadSettings.streaming_order.slice() : (downloadSettings.streaming_sources || []).map(function (step) { return step.name; });
        }
        var cookieResponse = await fetch("/v1/settings/cookies", {headers: {Accept: "application/json"}});
        var cookieBody = await cookieResponse.json();
        cookieSettings = cookieBody && Array.isArray(cookieBody.sites) ? cookieBody.sites : [];
        var aiResponse = await fetch("/v1/settings/ai", {headers: {Accept: "application/json"}});
        var aiBody = await aiResponse.json();
        aiSettings = aiBody && aiBody.ok ? aiBody.ai : null;
        var qbtResponse = await fetch("/v1/settings/qbittorrent", {headers: {Accept: "application/json"}});
        var qbtBody = await qbtResponse.json();
        qbtSettings = qbtBody && qbtBody.ok ? qbtBody.qbittorrent : null;
        var solverResponse = await fetch("/v1/settings/solver", {headers: {Accept: "application/json"}});
        var solverBody = await solverResponse.json();
        solverSettings = solverBody && solverBody.ok ? solverBody.solver : null;
        var lyricsResponse = await fetch("/v1/settings/lyrics", {headers: {Accept: "application/json"}});
        var lyricsBody = await lyricsResponse.json();
        lyricsSettings = lyricsBody && lyricsBody.ok ? lyricsBody.lyrics : null;
        var discogsResponse = await fetch("/v1/settings/discogs", {headers: {Accept: "application/json"}});
        var discogsBody = await discogsResponse.json();
        discogsSettings = discogsBody && discogsBody.ok ? discogsBody.discogs : null;
        var libraryResponse = await fetch("/v1/settings/library", {headers: {Accept: "application/json"}});
        var libraryBody = await libraryResponse.json();
        librarySettings = libraryBody && libraryBody.ok ? libraryBody.library : null;
        var serverResponse = await fetch("/v1/settings/servers", {headers: {Accept: "application/json"}});
        var serverBody = await serverResponse.json();
        serverSettings = serverBody && serverBody.ok ? serverBody.servers : null;
        var savedPlaylistResponse = await fetch("/v1/playlists", {headers: {Accept: "application/json"}});
        var savedPlaylistBody = await savedPlaylistResponse.json();
        playlistRows = savedPlaylistBody && Array.isArray(savedPlaylistBody.playlists) ? savedPlaylistBody.playlists : [];
        var subscriptionResponse = await fetch("/v1/subscriptions", {headers: {Accept: "application/json"}});
        var subscriptionBody = await subscriptionResponse.json();
        subscriptionRows = subscriptionBody && Array.isArray(subscriptionBody.subscriptions) ? subscriptionBody.subscriptions : [];
        var migrationResponse = await fetch("/v1/migration", {headers: {Accept: "application/json"}});
        var migrationBody = await migrationResponse.json();
        migrationReport = migrationBody && migrationBody.ok ? migrationBody : null;
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
        logEvents = logBody && Array.isArray(logBody.events) ? logBody.events : [];
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
