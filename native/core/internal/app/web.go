package app

const appJS = `(function () {
  var titles = {tasks: "任务", queue: "队列", review: "核对", search: "搜索", sites: "站点", settings: "设置", library: "库", logs: "日志"};
  var subtitles = {tasks: "从歌单识别、检索、下载到刮削入库，一处查看", queue: "正在运行和等待执行的工作", review: "自动核对未收敛的项目", search: "按目录和站点证据发起一次检索", sites: "全局站点访问边界", settings: "连接、下载、代理与维护策略", library: "整库刮削、去重、专辑补全与音质升级", logs: "任务、事件和运行记录"};
  var active = {preparing: true, identifying: true, snapshotting: true, metadata: true, searching: true, requesting: true, downloading: true, fallback: true, validating: true, publishing: true, notifying: true};
  var view = "tasks";
  var jobs = [];
  var activeCount = 0, pipelinePaused = false, globalJobCounts = null, autoReviewStatus = null;
  var currentJob = null;
  var currentActionJobID = "";
  var currentActionStage = "";
  var currentReviewJobID = "";
  var siteRows = [];
  var downloadSettings = null;
  var outboundProxy = null;
  var cookieSettings = [];
  var aiSettings = null;
  var qbtSettings = null;
  var solverSettings = null;
  var solverRuntime = null;
  var neteaseStatus = null;
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
  var duplicateScanStatus = null;
  var libraryFileOffset = 0;
  var libraryFileTotal = 0;
  var libraryPageFiles = [];
  var librarySelectAll = false;
  var selectedTracks = {};
  // The review view pages on the server: the last cursor is kept so more
  // pending jobs can be appended instead of replacing the list (P1-22).
  var jobsNext = "";
  var transcodeRows = [];
  var logRows = [];
  var selectedJobs = {};
  var loading = false;
  var lastLoadedAt = 0;
  var taskFilter = "active";
  var composerOpen = false;
  var status = document.getElementById("status");
  var list = document.getElementById("list");
  var note = document.getElementById("note");
  var detail = document.getElementById("detail");
  var heading = document.querySelector("h1");
  var subtitle = document.getElementById("view-subtitle");
  var viewLive = document.getElementById("view-live");
  var viewLiveLabel = document.getElementById("view-live-label");

  // One feedback channel for every action: kind ok/fail/review/info, and the
  // server's code when the server refused, instead of a generic failure.
  function setStatus(kind, text) {
    status.className = kind || "";
    status.textContent = text;
  }

  function retryAt(job) {
    if (!job) return 0;
    return Number(job.retry_after_ns || (job.root_status && job.root_status.retry_after_ns) || 0);
  }
  function retryPending(job) { return retryAt(job) > Date.now(); }
  function retryLabel(value) {
    var when = new Date(Number(value) / 1000000);
    return isNaN(when.getTime()) ? "等待重试" : "等待至 " + when.toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"});
  }
  function autoReviewLabel() {
    if (!autoReviewStatus) return "自动 AI 核对状态未知";
    if (!autoReviewStatus.enabled) return "自动 AI 核对已停止";
    if (autoReviewStatus.active) return "自动 AI 核对正在运行";
    if (autoReviewStatus.next_run_ns) return "自动 AI 核对等待下一轮";
    return "自动 AI 核对空闲";
  }
  function duplicateScanLabel() {
    var status = duplicateScanStatus || null;
    if (!status) return "重复扫描未开始";
    if (status.running) {
      if (status.total > 0) return "正在低频扫描重复项 " + (status.checked || 0) + "/" + status.total;
      return status.message || "正在读取本地索引";
    }
    if (status.cancelled) return (status.message || "扫描已停止；检查点已保留") + (status.warning ? " · " + status.warning : "");
    if (status.error) return "扫描失败：" + status.error + (status.warning ? " · " + status.warning : "");
    return "扫描完成 · " + (status.groups || 0) + " 组" + (status.warning ? " · " + status.warning : "");
  }
  function pill(job) {
    var progress = retryPending(job) ? retryLabel(retryAt(job)) : (job.progress || "未下载");
    var moving = retryPending(job) || progress.indexOf("处理中") >= 0 || progress.indexOf("正在") === 0 || progress === "检索中" || progress === "获取中" || progress === "识别中" || progress === "已排队流媒体" || progress === "正在入库";
    var kind = progress === "待核对" ? "review" : (progress === "未能继续" ? "fail" : (progress === "部分入库" ? "partial" : (moving ? "queue" : "")));
    return '<span class="pill ' + kind + '">' + escapeText(progress) + "</span>";
  }
  function escapeText(value) {
    return String(value == null ? "" : value).replace(/[&<>"]/g, function (ch) {
      return {"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;"}[ch];
    });
  }
  function roots() { return jobs.filter(function (job) { return !job.parent_job_id; }); }
  function taskRepresentativeRank(job) {
    if (job.execution_state === "running") return 0;
    if (job.stage === "requesting" || job.stage === "downloading" || job.stage === "validating" || job.stage === "publishing" || job.stage === "notifying") return 1;
    if (job.stage === "fallback") return 2;
    if (active[job.stage] || job.execution_state === "queued") return 3;
    if (job.stage === "needs_review") return 4;
    if (job.stage === "failed" || job.execution_state === "failed") return 5;
    return 6;
  }
  function taskRows() {
    // The API can add live descendants beside the root page so an active album
    // is never hidden behind old history. Collapse every family here as well:
    // one stable root row, represented by its most actionable live task.
    var groups = {}, order = [];
    jobs.forEach(function (job) {
      var root = job.root_id || job.job_id;
      if (!root) return;
      if (!groups[root]) {
        groups[root] = [];
        order.push(root);
      }
      groups[root].push(job);
    });
    return order.map(function (root) {
      var group = groups[root];
      var rep = group[0];
      for (var i = 1; i < group.length; i++) {
        var candidate = group[i];
        var candidateRank = taskRepresentativeRank(candidate);
        var repRank = taskRepresentativeRank(rep);
        if (candidateRank < repRank ||
            candidateRank === repRank && Number(candidate.updated_ns || 0) > Number(rep.updated_ns || 0) ||
            candidateRank === repRank && Number(candidate.updated_ns || 0) === Number(rep.updated_ns || 0) && candidate.job_id === root) {
          rep = candidate;
        }
      }
      var row = Object.assign({}, rep);
      row.job_id = root;
      row.root_id = root;
      row.parent_job_id = "";
      return row;
    });
  }
  function stageLabel(stage) {
    var names = {preparing: "准备", identifying: "识别", snapshotting: "读取身份", metadata: "元数据", searching: "检索", requesting: "请求", downloading: "下载", fallback: "流媒体回退", validating: "校验", publishing: "入库", notifying: "通知音乐服务", needs_review: "待核对", skipped: "已结束", completed: "已完成", cancelled: "已取消", failed: "失败"};
    return names[stage] || stage || "";
  }
  function queueRows() { return jobs.filter(function (job) { return active[job.stage]; }); }
  function taskBucket(job) {
    if (!job) return "done";
    if (job.stage === "needs_review" || job.progress === "待核对") return "review";
    if (job.stage === "failed" || job.execution_state === "failed" || job.progress === "未能继续") return "failed";
    if (active[job.stage] || job.execution_state === "running" || job.execution_state === "queued" || retryPending(job)) return "active";
    return "done";
  }
  function taskCounts() {
    var running = 0, queued = 0, review = 0, failed = 0, finished = 0;
    roots().forEach(function (job) {
      var bucket = taskBucket(job);
      if (bucket === "active") {
        running++;
        if (job.execution_state !== "running") queued++;
      } else if (bucket === "review") review++;
      else if (bucket === "failed") failed++;
      else finished++;
    });
    return {total: roots().length, running: running - queued, executing: running - queued, queued: queued, review: review, failed: failed, finished: finished};
  }
  function activeTaskCount(counts) {
    return checkCount(counts, "running") + checkCount(counts, "queued");
  }
  function taskFilterRows(rows) {
    if (view !== "tasks" || taskFilter === "all") return rows;
    return rows.filter(function (job) { return taskBucket(job) === taskFilter; });
  }
  function displayNumber(value) { return Number(value || 0).toLocaleString("zh-CN"); }
  function updateLiveChrome() {
    var visible = view === "tasks" || view === "queue" || view === "review";
    if (viewLive) viewLive.hidden = !visible;
    if (!viewLive) return;
    var queued = checkCount(globalJobCounts || taskCounts(), "queued");
    var waiting = !pipelinePaused && activeCount === 0 && queued > 0;
    viewLive.className = "view-live" + (pipelinePaused ? " paused" : (activeCount > 0 ? " running" : (waiting ? " waiting" : " idle")));
    if (viewLiveLabel) {
      viewLiveLabel.textContent = pipelinePaused ? "后台已暂停" : (activeCount > 0 ? displayNumber(activeCount) + " 项运行中" : (waiting ? displayNumber(queued) + " 项等待重试" : "后台待命"));
    }
  }
  function pipelineStages() {
    var groups = [
      {label:"识别", stages:{preparing:true, identifying:true, snapshotting:true, metadata:true}},
      {label:"检索", stages:{searching:true}},
      {label:"回退", stages:{fallback:true}},
      {label:"请求", stages:{requesting:true}},
      {label:"下载", stages:{downloading:true}},
      {label:"校验", stages:{validating:true}},
      {label:"入库", stages:{publishing:true, notifying:true}}
    ];
    var seen = {}, counts = groups.map(function () { return 0; });
    taskRows().forEach(function (job) {
      var id = job.root_id || job.job_id;
      if (seen[id] || !active[job.stage]) return;
      seen[id] = true;
      for (var i = 0; i < groups.length; i++) {
        if (groups[i].stages[job.stage]) { counts[i]++; break; }
      }
    });
    return '<ol class="pipeline-track" aria-label="处理阶段">' + groups.map(function (group, index) {
      var current = counts[index] > 0 ? " is-active" : "";
      return '<li class="pipeline-stage' + current + '"><span class="pipeline-index">' + String(index + 1).padStart(2, "0") + '</span><span class="pipeline-name">' + group.label + '</span><span class="pipeline-count">' + (counts[index] ? displayNumber(counts[index]) : "—") + '</span></li>';
    }).join("") + '</ol>';
  }
  function checkCount(counts, name) { return counts && counts[name] != null ? counts[name] : 0; }
  function taskSort(left, right) {
    var order = {active:0, review:1, failed:2, done:3};
    var leftOrder = order[taskBucket(left)];
    var rightOrder = order[taskBucket(right)];
    var byState = (leftOrder == null ? 3 : leftOrder) - (rightOrder == null ? 3 : rightOrder);
    if (byState) return byState;
    var running = (right.execution_state === "running" ? 1 : 0) - (left.execution_state === "running" ? 1 : 0);
    if (running) return running;
    return Number(right.updated_ns || 0) - Number(left.updated_ns || 0);
  }
  function visible() {
    var rows = [];
    if (view === "tasks") rows = taskRows();
    else if (view === "queue") rows = queueRows();
    else if (view === "review") rows = jobs.filter(function (job) { return job.stage === "needs_review"; });
    return rows.slice().sort(taskSort);
  }
  function setBusy(next) {
    loading = next;
    document.getElementById("main").setAttribute("aria-busy", next ? "true" : "false");
    document.querySelectorAll("#nav button[data-view], #back").forEach(function (button) { button.disabled = next; });
  }
  function executionLabel(state, paused, retryAfterNS) {
    if (paused) return "已暂停";
    if (Number(retryAfterNS || 0) > Date.now()) return retryLabel(retryAfterNS);
    return ({queued:"排队中", running:"运行中", paused:"已暂停", failed:"已失败", cancelled:"已取消", blocked:"待核对", recovery_required:"已中断", completed:"已完成"})[state] || "";
  }
  function reviewJobIDs(job) {
    if (!job) return [];
    if (Array.isArray(job.review_job_ids) && job.review_job_ids.length) {
      return job.review_job_ids.filter(Boolean);
    }
    if (!job.ai_review_eligible) return [];
    return [(job.root_status && job.root_status.job_id) || job.job_id].filter(Boolean);
  }
  function actionJobIDs(job, action) {
    if (!job) return [];
    if (view === "review" && action === "retry") {
      var reviews = reviewJobIDs(job);
      if (reviews.length) return reviews;
    }
    return [(job.root_status && job.root_status.job_id) || job.job_id].filter(Boolean);
  }
  function operationsPanel(counts) {
    var running = displayNumber(activeCount);
    var queued = displayNumber(checkCount(counts, "queued"));
    var waiting = !pipelinePaused && activeCount === 0 && checkCount(counts, "queued") > 0;
    var stateClass = pipelinePaused ? "paused" : (activeCount > 0 ? "running" : (waiting ? "waiting" : "idle"));
    var stateTitle = pipelinePaused ? "后台执行已暂停" : (activeCount > 0 ? "流水线正在运行" : (waiting ? "后台正在等待重试" : "后台待命"));
    var stateCopy = pipelinePaused ? "未开始的任务会保留在队列中，继续后可接着处理。" : (activeCount > 0 ? "任务会依次经过识别、检索、回退、下载、校验和入库。" : (waiting ? queued + " 项正在冷却或等待空闲线程，到点后会自动继续。" : "导入歌单或启动整库维护后，这里会显示实时进度。"));
    return '<section class="ops-overview ' + stateClass + '" id="ops-overview" aria-label="运行总览">' +
      '<div class="ops-main"><div class="ops-state"><span class="live-dot" aria-hidden="true"></span><span>' + (pipelinePaused ? "已暂停" : (activeCount > 0 ? "运行中" : (waiting ? "等待中" : "待命"))) + '</span></div>' +
      '<div class="ops-copy"><h2 id="ops-live-title">' + stateTitle + '</h2><p id="ops-live-copy">' + stateCopy + '</p></div>' +
      '<div class="ops-actions"><button type="button" id="pipeline-pause"' + (pipelinePaused ? " disabled" : "") + '>暂停调度</button><button type="button" id="pipeline-resume" class="primary"' + (!pipelinePaused ? " disabled" : "") + '>继续调度</button></div></div>' +
      '<div class="ops-metrics">' +
      '<div><span class="metric-value" id="stat-active">' + running + '</span><span class="metric-label">正在执行</span></div>' +
      '<div><span class="metric-value" id="stat-queued">' + displayNumber(checkCount(counts, "queued")) + '</span><span class="metric-label">等待执行</span></div>' +
      '<div><span class="metric-value" id="stat-review">' + displayNumber(checkCount(counts, "review")) + '</span><span class="metric-label">待核对</span></div>' +
      '<div><span class="metric-value" id="stat-failed">' + displayNumber(checkCount(counts, "failed")) + '</span><span class="metric-label">需处理</span></div>' +
      '<div><span class="metric-value" id="stat-finished">' + displayNumber(checkCount(counts, "finished")) + '</span><span class="metric-label">已完成</span></div>' +
      '</div>' + pipelineStages() +
      '<p class="ops-foot" id="pipeline-status">调度' + (pipelinePaused ? "已暂停" : "已开启") + ' · ' + autoReviewLabel() + ' · <span id="loaded-at">' + loadedAtLabel() + '</span></p></section>';
  }
  function filterBar(counts, rows, shown) {
    var modes = [
      {id:"all", label:"全部", count:checkCount(counts, "total")},
      {id:"active", label:"执行中", count:activeTaskCount(counts)},
      {id:"review", label:"待核对", count:checkCount(counts, "review")},
      {id:"failed", label:"失败", count:checkCount(counts, "failed")},
      {id:"done", label:"已完成", count:checkCount(counts, "finished")}
    ];
    return '<div class="task-filter-bar"><div class="task-filters" role="group" aria-label="筛选任务">' + modes.map(function (mode) {
      var pressed = taskFilter === mode.id ? "true" : "false";
      return '<button type="button" data-task-filter="' + mode.id + '" aria-pressed="' + pressed + '">' + mode.label + '<span>' + displayNumber(mode.count) + '</span></button>';
    }).join("") + '</div><span class="loaded-count">显示 ' + displayNumber(shown.length) + ' · 已载入 ' + displayNumber(rows.length) + ' 条' + (jobsNext ? " · 可继续加载" : "") + '</span></div>';
  }
  function loadedAtLabel() {
    if (!lastLoadedAt) return "正在读取";
    var seconds = Math.max(0, Math.round((Date.now() - lastLoadedAt) / 1000));
    if (seconds < 8) return "刚刚更新";
    if (seconds < 60) return seconds + " 秒前更新";
    return Math.floor(seconds / 60) + " 分钟前更新";
  }
  function formatTaskTime(value) {
    var ns = Number(value || 0);
    if (!ns) return "";
    var date = new Date(ns / 1000000);
    if (isNaN(date.getTime())) return "";
    return date.toLocaleString([], {month:"2-digit", day:"2-digit", hour:"2-digit", minute:"2-digit"});
  }
  function emptyResults(rows) {
    var text = taskFilter === "all" ? "还没有任务。" : "当前筛选下没有任务。";
    return '<div class="empty-state"><h2>' + text + '</h2><p>' + (taskFilter === "all" ? "导入歌单后，任务会自动进入这条流水线。" : "切换到全部任务查看其他状态。") + '</p>' + (taskFilter === "all" ? "" : '<button type="button" data-task-filter="all">查看全部任务</button>') + '</div>';
  }
  function reviewSummary(counts) {
    var count = checkCount(counts, "review");
    return '<section class="review-summary"><div><span class="section-kicker">自动核对</span><h2>' + (count ? displayNumber(count) + ' 项等待判断' : '没有待核对任务') + '</h2><p>' + autoReviewLabel() + '。单项可在任务详情里处理；批量核对会逐项提交并显示最终去向。</p></div><button type="button" id="review-ai-all" class="primary"' + (count ? "" : " disabled") + '>AI 核对全部</button></section>';
  }
  function taskCard(job) {
    var checked = selectedJobs[job.job_id] ? " checked" : "";
    var bucket = taskBucket(job);
    var state = retryPending(job) ? "" : executionLabel(job.execution_state, job.paused, retryAt(job));
    var stage = stageLabel(job.stage);
    if (stage === job.progress) stage = "";
    var selectable = bucket !== "done";
    var control = selectable ? '<label class="task-select"><input type="checkbox" data-batch="' + escapeText(job.job_id) + '"' + checked + ' aria-label="选择 ' + escapeText(job.label || "任务") + '"><span aria-hidden="true"></span></label>' : '<span class="task-select placeholder" aria-hidden="true"></span>';
    return '<article class="task-item is-' + bucket + '">' + control +
      '<button type="button" class="card-link" data-job="' + escapeText(job.job_id) + '"><span class="task-main"><span class="task-top"><span class="task-title">' + escapeText(job.label || "未命名") + '</span><span class="task-status">' + pill(job) + (state && state !== job.progress ? '<span class="task-exec">' + escapeText(state) + '</span>' : '') + '</span></span>' +
      '<span class="task-summary">' + escapeText(job.headline || "等待下一步") + '</span><span class="task-foot"><span>' + escapeText(stage) + '</span><span>' + escapeText(formatTaskTime(job.updated_ns)) + '</span></span></span><span class="task-caret" aria-hidden="true">›</span></button></article>';
  }  async function reviewAllAI() {
    if (loading) return;
    setBusy(true);
    var ids = new Set(), checked = 0, submitted = 0, settled = 0, held = 0, failed = 0, already = 0, failures = {};
    try {
      var cursor = "";
      setStatus("info", "正在读取全部待核对任务");
      do {
        var response = await fetch("/v1/jobs?review=1&after=" + encodeURIComponent(cursor), {headers: {Accept: "application/json"}});
        if (!response.ok) throw new Error("读取待核对任务失败");
        var page = await response.json();
        (page.jobs || []).forEach(function(job) { reviewJobIDs(job).forEach(function(id) { ids.add(id); }); });
        cursor = page.next || "";
      } while (cursor);
      if (!ids.size) {
        await reloadThen("ok", "没有待核对任务。");
        return;
      }
      var index = 0;
      for (var id of ids) {
        index++;
        setStatus("info", "AI 核对中 " + index + " / " + ids.size);
        var aiResponse = await fetch("/v1/jobs/" + encodeURIComponent(id) + "/review", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({action:"ai"})});
        var result = await aiResponse.json();
        if (result.code === "AI_UNAVAILABLE") throw new Error(result.message || "AI 未启用");
        if (result.already_processed) { already++; continue; }
        if (!aiResponse.ok && result.code === "AI_NO_CONFIRMED_MATCH") {
          checked++;
          var skipResponse = await fetch("/v1/jobs/" + encodeURIComponent(id) + "/review", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({action:"skip"})});
          var skipResult = await skipResponse.json();
          if (skipResponse.ok && skipResult.ok) {
            if (skipResult.held || skipResult.stage === "needs_review") held++;
            else if (skipResult.stage === "skipped") settled++;
            else submitted++;
          } else {
            failed++;
            var skipReason = skipResult.code || ("HTTP " + skipResponse.status);
            failures[skipReason] = (failures[skipReason] || 0) + 1;
          }
          continue;
        }
        if (!aiResponse.ok || !result.ok) {
          failed++;
          var reason = result.code || ("HTTP " + aiResponse.status);
          failures[reason] = (failures[reason] || 0) + 1;
          continue;
        }
        checked++;
        if (result.held || result.stage === "needs_review") held++;
        else if (result.stage === "skipped") settled++;
        else submitted++;
      }
      var details = Object.keys(failures).map(function(code) { return code + " " + failures[code] + " 项"; }).join(" · ");
      await reloadThen(failed ? "review" : "ok", "核对完成：检查 " + checked + " 项，继续处理 " + submitted + " 项，自动结束 " + settled + " 项，等待状态对账 " + held + " 项，失败 " + failed + " 项，此前已处理 " + already + " 项。" + (details ? " " + details : ""));
    } catch(err) {
      setStatus("fail", err.message + "。已检查 " + checked + " 项，继续处理 " + submitted + " 项。");
    } finally { setBusy(false); }
  }
  function render() {
    heading.textContent = titles[view];
    if (subtitle) subtitle.textContent = subtitles[view] || "";
    updateLiveChrome();
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
    var counts = globalJobCounts || taskCounts();
    var shown = taskFilterRows(rows);
    setStatus("", "");
    var html = "";
    if (view === "tasks") {
      html += operationsPanel(counts) + newTaskCard() + filterBar(counts, rows, shown);
    } else if (view === "queue") {
      html += operationsPanel(counts);
    } else {
      html += reviewSummary(counts);
    }
    html += batchBar();
    if (shown.length) {
      html += '<section class="task-list" aria-label="' + escapeText(titles[view]) + '列表">' + shown.map(taskCard).join("") + '</section>';
    } else {
      html += emptyResults(rows);
    }
    if ((view === "review" || view === "tasks") && jobsNext) html += '<div class="pagination"><button type="button" id="review-more">加载更多任务</button><span>当前已载入 ' + displayNumber(rows.length) + ' 条</span></div>';
    list.innerHTML = html;
    if (view === "tasks" || view === "queue") bindPipeline();
    if (view === "tasks") bindPlaylists();
    var aiAll = document.getElementById("review-ai-all");
    if (aiAll) aiAll.addEventListener("click", reviewAllAI);
    scheduleQueueRefresh();
  }
  function updateOpsState(body) {
    var counts = body && body.counts ? body.counts : {};
    globalJobCounts = counts;
    activeCount = Number(body && body.active_count || 0);
    pipelinePaused = !!(body && body.pipeline_paused);
    autoReviewStatus = (body && body.auto_review) || autoReviewStatus;
    var values = {active:activeCount, queued:checkCount(counts, "queued"), review:checkCount(counts, "review"), failed:checkCount(counts, "failed"), finished:checkCount(counts, "finished")};
    Object.keys(values).forEach(function (name) {
      var node = document.getElementById("stat-" + name);
      if (node) node.textContent = displayNumber(values[name]);
    });
    var filterCounts = {all:checkCount(counts, "total"), active:activeTaskCount(counts), review:checkCount(counts, "review"), failed:checkCount(counts, "failed"), done:checkCount(counts, "finished")};
    document.querySelectorAll("button[data-task-filter]").forEach(function (button) {
      var badge = button.querySelector("span");
      var value = filterCounts[button.getAttribute("data-task-filter")];
      if (badge && value != null) badge.textContent = displayNumber(value);
    });
    var statusLine = document.getElementById("pipeline-status");
    if (statusLine) statusLine.innerHTML = "调度" + (pipelinePaused ? "已暂停" : "已开启") + " · " + autoReviewLabel() + " · <span id=\"loaded-at\">" + loadedAtLabel() + "</span>";
    var pause = document.getElementById("pipeline-pause");
    if (pause) pause.disabled = pipelinePaused;
    var resume = document.getElementById("pipeline-resume");
    if (resume) resume.disabled = !pipelinePaused;
    var queued = checkCount(counts, "queued");
    var waiting = !pipelinePaused && activeCount === 0 && queued > 0;
    var title = document.getElementById("ops-live-title");
    if (title) title.textContent = pipelinePaused ? "后台执行已暂停" : (activeCount > 0 ? "流水线正在运行" : (waiting ? "后台正在等待重试" : "后台待命"));
    var copy = document.getElementById("ops-live-copy");
    if (copy) copy.textContent = pipelinePaused ? "未开始的任务会保留在队列中，继续后可接着处理。" : (activeCount > 0 ? "任务会依次经过识别、检索、回退、下载、校验和入库。" : (waiting ? displayNumber(queued) + " 项正在冷却或等待空闲线程，到点后会自动继续。" : "导入歌单或启动整库维护后，这里会显示实时进度。"));
    updateLiveChrome();
  }
  // The queue is a live view: counts refresh without disturbing a collapsed
  // composer, while the full queue refreshes on the same cadence.
  var queueTimer = null;
  function scheduleQueueRefresh() {
    if (queueTimer) { clearTimeout(queueTimer); queueTimer = null; }
    if (view !== "queue" && view !== "tasks") return;
    queueTimer = setTimeout(async function () {
      if (view === "queue") {
        await load(true);
      } else if (view === "tasks" && !loading && detail.hidden) {
        try {
          var response = await fetch("/v1/jobs?counts=only", {headers: {Accept: "application/json"}});
          if (response.ok && view === "tasks") {
            updateOpsState(await response.json());
            if ((activeCount > 0 || checkCount(globalJobCounts || {}, "queued") > 0) && Date.now() - lastLoadedAt > 4000) await load(true);
          }
        } catch (err) { /* The next refresh retries a transient connection failure. */ }
      }
      scheduleQueueRefresh();
    }, (pipelinePaused || (activeCount === 0 && checkCount(globalJobCounts || {}, "queued") === 0)) ? 10000 : 3000);
  }
  // The composer stays out of the way until someone needs to import a list.
  function newTaskCard() {
    return '<details class="task-composer" id="task-composer"' + (composerOpen ? ' open' : '') + '><summary><span class="composer-mark" aria-hidden="true"></span><span class="composer-copy"><strong>导入歌单</strong><small>粘贴平台链接、M3U 或表格；可同时创建整专补全任务</small></span><span class="composer-toggle" id="composer-toggle-label">' + (composerOpen ? "收起" : "展开") + '</span></summary><div class="composer-body">' + playlistsCard() + '</div></details>';
  }
  function bindPipeline() {
    var pause = document.getElementById("pipeline-pause");
    var resume = document.getElementById("pipeline-resume");
    if (pause) pause.addEventListener("click", function () { postSetting("/v1/control", {action: "pause"}).then(function () { return reloadThen("ok", "后台已暂停。未开始的任务保持排队，不会新扣令牌。"); }).catch(function () { setStatus("fail", "暂停失败。"); }); });
    if (resume) resume.addEventListener("click", function () { postSetting("/v1/control", {action: "resume"}).then(function () { return reloadThen("ok", "后台已继续。"); }).catch(function () { setStatus("fail", "继续失败。"); }); });
  }
  function selectedCount() {
    var count = 0;
    Object.keys(selectedJobs).forEach(function (id) { if (selectedJobs[id]) count++; });
    return count;
  }
  function updateBatchCount() {
    var bar = document.querySelector(".batch-count");
    if (bar) bar.textContent = "已选 " + selectedCount() + " 个";
  }
  function setBatchSelection(mode) {
    var byID = {};
    jobs.forEach(function (job) { byID[job.job_id] = job; });
    Object.keys(selectedJobs).forEach(function (id) { selectedJobs[id] = false; });
    document.querySelectorAll("input[data-batch]").forEach(function (box) {
      var id = box.getAttribute("data-batch");
      var job = byID[id] || {};
      var retryable = job.stage === "needs_review" || job.stage === "failed";
      var selected = mode === "all" || (mode === "retryable" && retryable);
      selectedJobs[id] = selected;
      box.checked = selected;
    });
    updateBatchCount();
  }
  function batchBar() {
    var count = selectedCount();
    return '<div class="batch" aria-label="批量操作"><span class="batch-count">已选 ' + count + ' 个</span>' +
      '<div class="batch-selection"><button type="button" data-batch-select="all">全选已显示</button>' +
      '<button type="button" data-batch-select="retryable">选择可处理项</button>' +
      '<button type="button" data-batch-select="none" class="quiet">清空</button></div>' +
      '<div class="batch-actions"><button type="button" data-batch-action="resume">批量继续</button>' +
      '<button type="button" data-batch-action="pause">批量暂停</button>' +
      '<button type="button" data-batch-action="retry">批量重试</button>' +
      '<button type="button" data-batch-action="cancel" class="danger">批量取消</button></div></div>';
  }
  async function batchAction(action) {
    var selectedRoots = Object.keys(selectedJobs).filter(function (id) { return selectedJobs[id]; });
    var byID = {};
    jobs.forEach(function (job) { byID[job.job_id] = job; });
    var ids = [];
    var seen = {};
    selectedRoots.forEach(function (rootID) {
      actionJobIDs(byID[rootID] || {job_id: rootID}, action).forEach(function (id) {
        if (!seen[id]) { seen[id] = true; ids.push(id); }
      });
    });
    if (!ids.length) { setStatus("fail", "先勾选要批量操作的任务。"); return; }
    if (loading) return;
    if ((action === "cancel" || action === "retry") && !window.confirm(action === "cancel" ? "取消选中的 " + ids.length + " 个任务吗？取消后未开始的阶段不会继续。" : "重试选中的 " + ids.length + " 个任务吗？")) return;
    setBusy(true);
    setStatus("info", "正在批量处理");
    try {
      var succeeded = 0, failedCount = 0, failed = [];
      for (var start = 0; start < ids.length; start += 50) {
        var chunk = ids.slice(start, start + 50);
        setStatus("info", "正在批量处理 " + Math.min(start + chunk.length, ids.length) + " / " + ids.length);
        var response = await fetch("/v1/jobs/batch", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({job_ids: chunk, action: action})});
        var body = await response.json();
        if (!response.ok || !body || !body.ok) {
          setStatus("fail", "批量操作失败。" + (body && body.code ? "（" + body.code + "）" : ""));
          return;
        }
        succeeded += Number(body.succeeded || 0);
        failedCount += Number(body.failed || 0);
        (body.results || []).forEach(function (item) { if (!item.ok) failed.push(item.job_id + "：" + (item.code || "失败")); });
      }
      Object.keys(selectedJobs).forEach(function (id) { selectedJobs[id] = false; });
      if (failedCount) await reloadThen("review", "批量完成：成功 " + succeeded + " 个，失败 " + failedCount + " 个。" + failed.slice(0, 8).join("；") + (failed.length > 8 ? "；另有 " + (failed.length - 8) + " 项" : ""));
      else await reloadThen("ok", "批量完成：成功 " + succeeded + " 个。");
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
    var html = '<div class="cards"><article><h2>全局允许站点</h2><p class="meta">这里是四站的全局允许列表，会同时约束新任务和已经排队的旧任务。全部取消并保存后，不会再用四站检索、读取详情或下载种子；网易云等流媒体回退不受影响。</p>';
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
      render();
      setStatus("ok", "已保存。");
    } catch (err) {
      setStatus("fail", "保存失败。");
    } finally {
      setBusy(false);
    }
  }
  function renderSearch() {
    setStatus("", "");
    var html = '<div class="cards"><article><form id="search-form"><h2>提交搜索</h2>' +
      '<p class="meta">只读检索全局允许的四站；任务自己的站点选择不能绕过全局允许列表。流媒体回退由设置页单独控制。</p>' +
      '<label>检索关键词<input type="text" id="search-query" required></label>' +
      '<label>期望标题<input type="text" id="search-title" required></label>' +
      '<label>期望艺人<input type="text" id="search-artist"></label>' +
      '<label>场景<select id="search-scene"><option value="playlist">歌单</option><option value="upgrade">音质升级</option><option value="fill">补缺</option><option value="expand">扩展</option></select></label>' +
      '<label>来源曲目 ID（歌单场景，逗号分隔）<input type="text" id="search-tracks"></label>' +
      '<label>已存歌单（歌单场景：回退时读该歌单文件）<select id="search-playlist"><option value="">不使用已存歌单</option>' + playlistRows.map(function (row) { return '<option value="' + escapeText(row.id) + '">' + escapeText((row.name || row.id) + "（" + row.track_count + " 首）") + "</option>"; }).join("") + '</select></label>' +
      '<label>流媒体回退<select id="search-streaming"><option value="">跟随设置</option><option value="true">开</option><option value="false">关</option></select></label>' +
      '<fieldset><legend>检索站点（始终与站点页的全局允许列表取交集；都不选则使用全局允许列表）</legend><span id="search-sites"></span></fieldset>' +
      '<fieldset><legend>身份证据（来自目录的才算确认；AI 或合成结果不能确认身份）</legend>' +
      '<label>证据来源<select id="search-identity-kind"><option value="catalog">目录</option><option value="api">接口</option><option value="website">网站</option></select></label>' +
      '<label>来源名称<input type="text" id="search-identity-source" placeholder="如 musicbrainz" value="musicbrainz"></label>' +
      '<label>来源条目 ID<input type="text" id="search-identity-subject" placeholder="如 MusicBrainz Release ID"></label>' +
      '<label>备注<input type="text" id="search-identity-payload" placeholder="可选，如核对过的曲目数"></label>' +
      '<label><input type="checkbox" id="search-identity-tracklist"> 已核对曲目清单</label>' +
      '<label><input type="checkbox" id="search-identity-confirmed"> 确认这是同一发行（须先核对曲目清单）</label></fieldset>' +
      '<label>本次检索的 Cookie（只用于这次检索，不保存；留空用已保存的 Cookie）<input type="password" id="search-cookie" autocomplete="off"></label>' +
      '<p><button type="submit" class="primary">提交搜索</button></p></form></article>' + downloadCard() + cookieCard() + neteaseCard() + '</div>';
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
    var body = {job_id: jobID, batch_id: jobID, scene: scene, query: query, expected_title: title, expected_artist: document.getElementById("search-artist").value.trim(), source_track_ids: tracks, identity: identity, cookie: document.getElementById("search-cookie").value, synthetic: false};
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
      view = "queue";
      await reloadThen("ok", "已提交搜索，任务 " + jobID + "。");
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
  function savedMark(saved) {
    // The revision is the vault's internal write counter; showing "版本 3"
    // next to a password only makes people think something is wrong.
    return saved ? "已保存" : "未保存";
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
    var statusByName = {};
    (downloadSettings.streaming_source_status || []).forEach(function (step) { statusByName[step.name] = step; });
    var orderHtml = streamingOrder.map(function (name, index) {
      var step = statusByName[name] || {};
      var note = step.enabled === false ? "（默认不启用，不会去请求）" : "";
      return '<p class="meta">' + escapeText(name) + escapeText(note) +
        ' <button type="button" data-order-move="' + index + ':-1">↑</button>' +
        ' <button type="button" data-order-move="' + index + ':1">↓</button></p>';
    }).join("");
    return '<article><h2>下载默认</h2><p class="meta">搜索时不指定流媒体回退，就用这里的选择。</p>' +
      '<label><input type="checkbox" id="download-streaming"' + (downloadSettings.streaming_enabled ? " checked" : "") + '> 流媒体回退（已启用的站点都没有精确匹配时，再尝试流媒体）</label>' +
      '<label>令牌下载预算（每个歌单批次，0 表示不花令牌）<input type="number" id="download-token-budget" min="0" max="100000" value="' + (downloadSettings.token_budget || 0) + '"></label>' +
      '<p class="meta">dicmusic、orpheus 几乎没有免费种子，正常下载就是用 FL 令牌。这里填每个歌单批次最多用多少个令牌；0 表示只用免费种子。流媒体引擎用随程序启动的内嵌引擎，不需要填地址（远程引擎用环境变量 MUSICOPS_ENGINE_URL）。</p>' +
      '<p><button type="button" id="save-download" class="primary">保存</button></p>' +
      '<p class="meta">流媒体回退顺序（可上下调整）：</p>' + orderHtml +
      '<p class="meta">实际会尝试的顺序是 Tidal、Apple Music、QQ 音乐、网易云，最后是已导入的歌单文件；网易云来源的歌单也遵守这里保存的顺序。Qobuz 和 Deezer 排在表里，但默认不启用。下载由随程序启动的内嵌引擎完成（go-music-dl），只有返回的文件通过音频校验才算下载，而且这时仍未入库。</p></article>';
  }
  // A staled cookie answers as a guest: search still works, the download
  // does not. Ask each site instead of letting the job fail much later.
  async function checkCookieLogins() {
    var out = document.getElementById("cookie-check-result");
    if (!out) return;
    out.textContent = "正在问四个站点…";
    try {
      var response = await fetch("/v1/settings/cookies/check", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        out.textContent = "检查失败。";
        return;
      }
      out.innerHTML = (body.sites || []).map(function (row) {
        return escapeText(row.site) + "：" + (row.logged_in ? "已登录" : "未登录") + "（" + escapeText(row.detail || "") + "）";
      }).join("<br>");
    } catch (err) {
      out.textContent = "检查中断：" + err.message;
    }
  }
  function cookieCard() {
    var rows = cookieSettings.map(function (site) {
      return '<label>' + escapeText(site.name) + '（' + escapeText(site.host || "") + '）· ' + escapeText(savedMark(site.saved, site.revision)) +
        '<textarea id="cookie-' + escapeText(site.name) + '" rows="3" placeholder="粘贴 Cookie-Editor JSON；留空保留已保存的值"></textarea></label>' +
        '<p><button type="button" data-cookie-site="' + escapeText(site.name) + '">保存 ' + escapeText(site.name) + ' Cookie</button></p>';
    }).join("");
    return '<article><h2>站点 Cookie</h2><p class="meta">每站的 Cookie 加密保存在本机，只显示“已保存”，不回显内容。搜索、登录检查和下载优先用这里保存的 Cookie；检索时临时填写的 Cookie 只用于那一次。</p>' + rows + '<p><button type="button" id="check-cookies">检查登录状态</button></p><p id="cookie-check-result" class="meta"></p></article>';
  }
  function proxyCard() {
    if (!outboundProxy) return "";
    var current = outboundProxy.enabled ? ("已启用：" + escapeText(outboundProxy.active_url || outboundProxy.url)) : "未配置代理，外部请求直连";
    return '<article><h2>出站代理</h2><p class="meta">核心、流媒体引擎、FlareSolverr 和桌面更新检查的外部 HTTP/HTTPS 请求统一走这里，保存后立即生效。访问 qBittorrent、FlareSolverr、内置引擎和本机 API 的 localhost、127.0.0.0/8、::1 地址仍会直连。可填完整地址，也可直接填“主机:端口”。</p>' +
      '<p class="meta">当前：' + current + '</p>' +
      '<label>代理地址<input type="text" id="proxy-url" value="' + escapeText(outboundProxy.url || "") + '" placeholder="192.168.31.11:7890" autocomplete="off"></label>' +
      '<p><button type="button" id="save-proxy" class="primary">保存并切换</button> <button type="button" id="test-proxy">测试连接</button></p>' +
      '<p class="meta">留空并保存会关闭代理。地址不得包含用户名、密码、路径或查询参数。</p><p id="proxy-test-result" class="meta">' + escapeText(outboundProxy.config_error || "") + '</p></article>';
  }
  function aiCard() {
    if (!aiSettings) return "";
    return '<article><h2>AI 设置</h2><p class="meta">启用后，后台会持续自动核对可安全重放的项目，并保留单项/全部按钮用于手动加跑。旧网易云任务先按持久化的歌曲与专辑 ID 自证；模型不能单独放行，仍要通过本地的标题、艺人和版本校验。DicMusic/Orpheus 的未确认令牌不会自动重试或消费，仍按 FL 规则等待明确决定。API Key 加密保存在本机，不回显。</p>' +
      '<label><input type="checkbox" id="ai-enabled"' + (aiSettings.enabled ? " checked" : "") + '> 启用自动 AI 核对</label>' +
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
    return '<article><h2>qBittorrent</h2><p class="meta">下载用的 qBittorrent 地址和登录。登录加密保存在本机，不回显。用户名和密码要一起填；密码留空则保留已保存的登录。</p>' +
      '<label>地址<input type="text" id="qbt-base-url" value="' + escapeText(qbtSettings.base_url || "") + '" placeholder="http://127.0.0.1:8080"></label>' +
      '<label>用户名<input type="text" id="qbt-username" autocomplete="off"></label>' +
      '<label>密码（' + escapeText(savedMark(qbtSettings.login_saved, qbtSettings.login_revision)) + '，留空保留）<input type="password" id="qbt-password" autocomplete="off"></label>' +
      '<label>分类<input type="text" id="qbt-category" value="' + escapeText(qbtSettings.category || "") + '"></label>' +
      '<label>标签<input type="text" id="qbt-tags" value="' + escapeText(qbtSettings.tags || "") + '"></label>' +
      '<p><button type="button" id="save-qbt" class="primary">保存</button> <button type="button" id="test-qbt">测试连接</button> <button type="button" id="unify-qbt-tags">统一旧标签</button></p><p id="qbt-test-result" class="meta"></p></article>';
  }
  function solverCard() {
    if (!solverSettings) return "";
    var runtime = solverRuntime && solverRuntime.state ? "当前：" + (solverRuntime.state === "ready" ? "可用" : (solverRuntime.state === "starting" ? "正在启动" : "启动失败")) + (solverRuntime.detail ? "，" + solverRuntime.detail : "") + (solverRuntime.version ? "，版本 " + solverRuntime.version : "") : "当前：未由本程序启动。";
    return '<article><h2>FlareSolverr</h2><p class="meta">浏览器验证服务，地址须为本机回环地址。只有遇到浏览器挑战的只读请求会交给它重试：直连仍是首选，登录失效、限流和取种子的请求不会被重放。启用后若勾选自动启动并填好可执行程序，程序启动时会自行拉起服务，退出时一并停止。</p>' +
      '<p class="meta">' + escapeText(runtime) + '</p>' +
      '<label><input type="checkbox" id="solver-managed"' + (solverSettings.managed ? " checked" : "") + '> 由引擎管理（用内置 FlareSolverr）</label>' +
      '<label><input type="checkbox" id="solver-enabled"' + (solverSettings.enabled ? " checked" : "") + '> 启用浏览器验证</label>' +
      '<label>地址<input type="text" id="solver-base-url" value="' + escapeText(solverSettings.base_url || "") + '"></label>' +
      '<label>超时（秒，10–120）<input type="text" id="solver-timeout" value="' + escapeText(solverSettings.timeout) + '"></label>' +
      '<label>可执行程序（仅 Windows，.exe 绝对路径）<input type="text" id="solver-executable" value="' + escapeText(solverSettings.executable || "") + '"></label>' +
      '<label><input type="checkbox" id="solver-auto-start"' + (solverSettings.auto_start ? " checked" : "") + '> 程序启动时一并启动（Windows，需填可执行程序）</label>' +
      '<p><button type="button" id="save-solver" class="primary">保存</button> <button type="button" id="test-solver">测试连接</button></p><p id="solver-test-result" class="meta"></p></article>';
  }
  function neteaseCard() {
    var saved = neteaseStatus && neteaseStatus.saved;
    return '<article><h2>内置网易云</h2><p class="meta">内置源：不需要外部引擎。保存账号 Cookie 后，网易云歌单会优先取无损 FLAC；四站等来源都没有时，账号只返回 320k MP3 也会转成带“有损来源”标记的 FLAC 完成入库，后续音质升级会继续寻找真正无损版本。同一份 Cookie 也用于读取网易云歌单。</p>' +
      '<p class="meta">当前：' + (saved ? "已保存账号 Cookie" : "未保存，内置网易云不会启用") + "</p>" +
      '<label>账号 Cookie（浏览器扩展导出的 JSON，只保存在本机加密库）<textarea id="netease-cookie" rows="3" placeholder="粘贴 Cookie-Editor 导出的 JSON"></textarea></label>' +
      '<p><button type="button" id="save-netease" class="primary">保存 Cookie</button></p><p id="netease-result" class="meta"></p></article>';
  }
  async function saveNetease() {
    var result = document.getElementById("netease-result");
    var value = document.getElementById("netease-cookie").value.trim();
    if (!value) { if (result) result.textContent = "先粘贴 Cookie。"; return; }
    try {
      var response = await fetch("/v1/settings/streaming/netease", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({cookie_json: value})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        if (result) result.textContent = (body && (body.error || body.code)) || "没有保存。";
        return;
      }
      document.getElementById("netease-cookie").value = "";
      if (result) result.textContent = body.saved ? "已保存。" + (body.playlist_ref === false ? "（歌单引用未写入，歌单读取可能仍需要旧凭据）" : "歌单读取也使用这份 Cookie。") : "没有保存。";
      load(true);
    } catch (err) {
      if (result) result.textContent = "保存中断。";
    }
  }
  function lyricsCard() {
    if (!lyricsSettings) return "";
    return '<article><h2>歌词与封面服务</h2><p class="meta">地址与路径对应 LrcApi 项目（lyrics_path 返回 LRC 文本，cover_path 返回图片）。主服务须为 HTTPS，本机回环可用 HTTP。鉴权值按原样保存，不会自动加上 Bearer 前缀，并且加密保存在本机——带凭据时不会跟随跳转，避免把凭据发到别的主机。保存后，补齐标签时会按曲名、艺人、专辑取歌词和封面；服务没有这一首（404）就跳过，网络失败则不写文件。</p>' +
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
    return '<article><h2>Discogs</h2><p class="meta">在线专辑识别用的 Token，加密保存在本机。保存后，目录识别会向 Discogs 查询候选；这些候选不能单独确认是哪一张发行。</p>' +
      '<label>Token（' + escapeText(savedMark(discogsSettings.token_saved, discogsSettings.token_revision)) + '，留空保留）<input type="password" id="discogs-token" autocomplete="off"></label>' +
      '<p><button type="button" id="save-discogs" class="primary">保存</button> <button type="button" id="test-discogs">测试连接</button></p><p id="discogs-test-result" class="meta"></p></article>';
  }
  function libraryCard() {
    if (!librarySettings) return "";
    return '<article><h2>音乐库目录</h2><p class="meta">新入库的音频放这里；改目录只影响新入库，已入库的文件不动，各自按入库时的目录解析。目录须为绝对路径，不能是磁盘根目录，不能与状态/备份目录重叠。</p>' +
      '<label>音乐库目录<input type="text" id="library-dir" value="' + escapeText(librarySettings.library_dir || "") + '" placeholder="留空用默认：' + escapeText(librarySettings.default_library_dir || "") + '"></label>' +
      '<p class="meta">当前生效：' + escapeText(librarySettings.effective_library_dir || "") + '</p>' +
      '<h2>临时下载与处理目录</h2><p class="meta">有独立 SSD 时填这里：下载、上传、整理副本和做种副本落在该目录，验证后的成品才写到音乐库。临时缓存上限只限制下载与整理副本；永久做种原件不计入该上限，也不会自动清理。留空沿用状态目录。只影响之后开始的任务，已在进行的任务沿用原路径；不能与音乐库重叠。</p>' +
      '<label>临时目录<input type="text" id="library-work-dir" value="' + escapeText(librarySettings.work_dir || "") + '" placeholder="留空用默认：状态目录"></label>' +
      '<p class="meta">当前生效：' + escapeText(librarySettings.effective_work_dir || "") + '</p>' +
      '<p><button type="button" id="save-library" class="primary">保存</button></p></article>';
  }
  function serversCard() {
    if (!serverSettings) return "";
    var plex = serverSettings.plex || {};
    var navidrome = serverSettings.navidrome || {};
    return '<article><h2>音乐服务器</h2><p class="meta">入库完成后通知服务器重新扫描：先 Navidrome，再 Plex。通知失败只写在任务说明里，不会挡住入库。Token 和密码加密保存在本机，不回显。</p>' +
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
      '<h2>Roon</h2><p class="meta">内嵌扩展：连接后在 Roon 的「设置 → 扩展」里把 MusicOps 打开一次，之后会记住。可以列播放区、播放/暂停/切歌、调音量。</p>' +
      '<label>主机<input type="text" id="roon-host" value="127.0.0.1"></label><label>端口<input type="number" id="roon-port" value="9330"></label>' +
      '<p><button type="button" id="roon-connect" class="primary">连接 Roon</button> <button type="button" id="roon-disconnect">断开</button></p>' +
      '<p id="roon-result" class="meta"></p><div id="roon-zones"></div></article>';
  }
  function playlistsCard() {
    var saved = playlistRows.map(function (row) {
      return '<div class="saved-playlist"><div><strong>' + escapeText(row.name || row.id) + '</strong><span>' + displayNumber(row.track_count) + ' 首' + (row.error ? ' · ' + escapeText(row.error) : '') + '</span></div><span class="saved-playlist-id">' + escapeText(row.id) + '</span></div>';
    }).join("");
    var subs = subscriptionRows.map(function (row) {
      return '<div class="saved-playlist"><div><strong>' + escapeText(row.name || row.playlist_id) + '</strong><span>每 ' + escapeText(row.interval_hours) + ' 小时 · ' + (row.paused ? '已暂停' : '运行中') + (row.last_result ? ' · ' + escapeText(row.last_result) : '') + '</span></div><div class="inline-actions"><button type="button" data-sub-check="' + escapeText(row.playlist_id) + '">立即检查</button><button type="button" data-sub-pause="' + escapeText(row.playlist_id) + '" data-paused="' + (row.paused ? "1" : "0") + '">' + (row.paused ? "恢复" : "暂停") + '</button><button type="button" data-sub-delete="' + escapeText(row.playlist_id) + '" class="quiet">删除</button></div></div>';
    }).join("");
    return '<div class="composer-grid"><section class="composer-section composer-section-primary"><div class="section-heading"><div><h2>从平台导入</h2><p class="section-copy">读取公开歌单的曲目信息，再把缺歌和目标专辑送进后台流水线。</p></div><span class="section-badge">自动处理</span></div>' +
      '<div class="field-grid"><label>平台<select id="remote-platform"><option value="netease">网易云音乐</option><option value="kugou">酷狗</option><option value="qishui">汽水</option><option value="migu">咪咕</option><option value="applemusic">Apple Music</option><option value="spotify">Spotify</option><option value="youtube">YouTube</option><option value="qqmusic">QQ 音乐</option><option value="kuwo">酷我</option></select></label><label>歌单链接或 ID<input type="text" id="remote-playlist-id" placeholder="粘贴歌单链接，或直接填 ID"></label></div>' +
      '<label>本机测试地址（可选）<input type="text" id="remote-base" placeholder="留空访问平台公开接口"></label>' +
      '<div class="option-grid composer-options"><label><input type="checkbox" id="playlist-enqueue" checked> 拉取后直接创建入库任务</label><label><input type="checkbox" id="playlist-album" checked> 同时补全缺失的整张专辑</label><label>入库范围<select id="playlist-mode"><option value="all">全部曲目</option><option value="pick">先选择部分曲目</option></select></label></div>' +
      '<div class="action-row"><button type="button" id="fetch-playlist" class="primary">拉取并创建入库任务</button><span class="form-note">不会直接打开歌单文件里的链接</span></div><p id="playlist-fetch-result" class="meta" aria-live="polite"></p><div id="playlist-pick"></div></section>' +
      '<section class="composer-section"><div class="section-heading"><div><h2>已存歌单</h2><p class="section-copy">' + (playlistRows.length ? '可在检索页作为流媒体回退来源。' : '还没有已存歌单。') + '</p></div><span class="section-count">' + displayNumber(playlistRows.length) + '</span></div><div class="saved-list">' + (saved || '<div class="inline-empty">保存歌单后会显示在这里。</div>') + '</div></section>' +
      '<details class="composer-group"><summary><span><strong>导入文件或文本</strong><small>M3U、CSV、JSON、TSV、TXT 或 XLSX</small></span><span class="summary-disclosure" aria-hidden="true"></span></summary><div class="composer-group-body"><div class="field-grid"><label>歌单名称<input type="text" id="playlist-name" placeholder="留空使用文件里的名称"></label><label>文本顺序<select id="playlist-order"><option value="artist-title">歌手 - 歌名</option><option value="title-artist">歌名 - 歌手</option></select></label></div><label>上传文件<input type="file" id="playlist-file" accept=".m3u,.m3u8,.json,.csv,.tsv,.txt,.xlsx"></label><label>或粘贴内容<textarea id="playlist-content" rows="4" placeholder="歌手 - 歌名，或带表头的 CSV / JSON"></textarea></label><div class="action-row"><button type="button" id="save-playlist" class="primary">保存歌单</button></div></div></details>' +
      '<details class="composer-group"><summary><span><strong>识别链接</strong><small>只在本地解析平台与歌单 ID</small></span><span class="summary-disclosure" aria-hidden="true"></span></summary><div class="composer-group-body"><label>分享链接或歌单 ID<input type="text" id="playlist-link" placeholder="https://music.163.com/playlist?id=123"></label><div class="action-row"><button type="button" id="locate-playlist">识别链接</button></div><p id="playlist-locate-result" class="meta"></p></div></details>' +
      '<details class="composer-group"><summary><span><strong>订阅歌单变化</strong><small>定时读取新增曲目，不移除库中已有音乐</small></span><span class="summary-disclosure" aria-hidden="true"></span></summary><div class="composer-group-body"><div class="field-grid"><label>歌单<select id="subscription-playlist"><option value="">选择已存歌单</option>' + playlistRows.map(function (row) { return '<option value="' + escapeText(row.id) + '">' + escapeText(row.name || row.id) + '</option>'; }).join("") + '</select></label><label>间隔（小时）<input type="number" id="subscription-hours" min="1" max="168" value="6"></label></div><label><input type="checkbox" id="subscription-existing"> 现有曲目也入库</label><div class="action-row"><button type="button" id="save-subscription" class="primary">订阅</button></div><div class="saved-list">' + (subs || '<div class="inline-empty">还没有订阅。</div>') + '</div></div></details></div>';
  }
  function migrationCard() {
    var report = migrationReport || {};
    var counts = report.report || {};
    var summary = report.found ? ("已迁入曲目 " + (counts.library || 0) + "，待核对任务 " + (counts.jobs || 0) + "，令牌记录 " + (counts.tokens || 0) + "。") : "还没有旧版活动数据回执。";
    return '<article><h2>从旧版迁入</h2><p class="meta">' + escapeText(report.note || "启动时会读取旧版数据库。音乐文件不移动。") + "</p><p>" + escapeText(summary) + "</p>" +
      '<p class="meta">旧库归档' + (report.cold_archive ? "仍保留" : "还没有") + "。自动安装更新：" + (report.auto_install ? "会" : "不会") + "。</p>" +
      '<p><button type="button" id="rollback-migration">回退迁入的索引</button></p><p id="migration-result" class="meta"></p></article>';
  }
  function renderSettings() {
    if (!downloadSettings && !outboundProxy && !cookieSettings.length && !aiSettings && !qbtSettings && !solverSettings && !lyricsSettings && !discogsSettings && !librarySettings && !serverSettings) {
      setStatus("", "设置没有读到。");
      list.innerHTML = "";
      return;
    }
    setStatus("", "");
    var html = '<div class="cards">' + migrationCard() + proxyCard() + aiCard() + qbtCard() + solverCard() + lyricsCard() + discogsCard() + libraryCard() + serversCard() +
      '<article><h2>站点登录检查</h2><p class="meta">只读检查：只验证登录和余额，不下载，也不消耗令牌。Cookie 留空则用已保存的；填写的只用于这次检查，不保存。</p>' +
      '<label>站点<select id="preflight-site">' + siteRows.map(function (site) { return '<option value="' + escapeText(site.name) + '">' + escapeText(site.name) + "</option>"; }).join("") + '</select></label>' +
      '<label>Cookie<input type="password" id="preflight-cookie" autocomplete="off"></label>' +
      '<p><button type="button" id="run-preflight" class="primary">检查登录</button></p><p id="preflight-result" class="meta"></p></article></div>';
    list.innerHTML = html;
    var save = document.getElementById("save-download");
    if (save) save.addEventListener("click", saveDownload);
    var saveProxyButton = document.getElementById("save-proxy");
    if (saveProxyButton) saveProxyButton.addEventListener("click", saveProxy);
    var testProxyButton = document.getElementById("test-proxy");
    if (testProxyButton) testProxyButton.addEventListener("click", testProxy);
    document.querySelectorAll("[data-order-move]").forEach(function (button) {
      button.addEventListener("click", function () {
        var parts = button.getAttribute("data-order-move").split(":");
        moveOrder(parseInt(parts[0], 10), parseInt(parts[1], 10));
      });
    });
    var checkCookies = document.getElementById("check-cookies");
    if (checkCookies) checkCookies.addEventListener("click", checkCookieLogins);
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
    var saveNeteaseButton = document.getElementById("save-netease");
    if (saveNeteaseButton) saveNeteaseButton.addEventListener("click", saveNetease);
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
    var roonConnect = document.getElementById("roon-connect");
    if (roonConnect) roonConnect.addEventListener("click", connectRoon);
    var roonDisconnect = document.getElementById("roon-disconnect");
    if (roonDisconnect) roonDisconnect.addEventListener("click", disconnectRoon);
    if (document.getElementById("roon-zones")) {
      refreshRoon();
      startRoonPolling();
    }
    bindPlaylists();
    var run = document.getElementById("run-preflight");
    if (run) run.addEventListener("click", preflight);
  }
  // bindPlaylists wires the playlist forms wherever they are rendered.
  function bindPlaylists() {
    var composer = document.getElementById("task-composer");
    if (composer) {
      composerOpen = composer.open;
      composer.addEventListener("toggle", function () {
        composerOpen = composer.open;
        var label = document.getElementById("composer-toggle-label");
        if (label) label.textContent = composerOpen ? "收起" : "展开";
      });
    }
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
    var enqueueHint = document.getElementById("playlist-enqueue");
    if (enqueueHint) enqueueHint.addEventListener("change", function () {
      var button = document.getElementById("fetch-playlist");
      if (button) button.textContent = enqueueHint.checked ? "拉取并创建入库任务" : "只拉取曲目名单";
    });
  }
  async function saveProxy() {
    if (loading) return;
    var field = document.getElementById("proxy-url");
    setBusy(true);
    setStatus("info", "正在保存并验证出站代理");
    try {
      var body = await postSetting("/v1/settings/proxy", {url: field ? field.value.trim() : ""});
      outboundProxy = body.proxy || null;
      renderSettings();
      if (!body.reachable && body.check) {
        setStatus("review", "代理已保存并切换，但连接检查失败：" + (body.check.error || "未知错误"));
      } else if (outboundProxy && outboundProxy.enabled) {
        setStatus("ok", "代理已保存并切换，外部连接探测成功。");
      } else {
        setStatus("ok", "出站代理已关闭。");
      }
    } catch (err) {
      setStatus("fail", "保存失败。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      setBusy(false);
    }
  }
  async function testProxy() {
    if (loading) return;
    var field = document.getElementById("proxy-url");
    var result = document.getElementById("proxy-test-result");
    setBusy(true);
    setStatus("info", "正在测试出站代理");
    try {
      var body = await postSetting("/v1/settings/proxy/test", {url: field ? field.value.trim() : ""});
      var text = body.message || "代理连接成功。";
      if (body.check && body.check.latency_ms != null) text += "耗时 " + body.check.latency_ms + " ms。";
      if (result) result.textContent = text;
      setStatus("ok", text);
    } catch (err) {
      var failed = "测试失败。" + (err && err.message ? "（" + err.message + "）" : "");
      if (result) result.textContent = failed;
      setStatus("fail", failed);
    } finally {
      setBusy(false);
    }
  }
  async function saveDownload() {
    if (loading) return;
    setBusy(true);
    setStatus("info", "正在保存");
    try {
      var budgetBox = document.getElementById("download-token-budget");
      var body = await postSetting("/v1/settings/download", {streaming_enabled: document.getElementById("download-streaming").checked, streaming_order: streamingOrder, token_budget: budgetBox ? (parseInt(budgetBox.value, 10) || 0) : 0});
      downloadSettings.streaming_enabled = body.streaming_enabled;
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
  // Roon: the app is itself the extension. Registering can wait for a person
  // to click Enable inside Roon, so the page polls the status instead of
  // holding a request open.
  var roonTimer = null;
  var roonZones = [];
  function startRoonPolling() {
    stopRoonPolling();
    roonTimer = setInterval(function () {
      if (!document.getElementById("roon-zones")) { stopRoonPolling(); return; }
      refreshRoon();
    }, 3000);
  }
  function stopRoonPolling() {
    if (roonTimer) { clearInterval(roonTimer); roonTimer = null; }
  }
  async function refreshRoon() {
    try {
      var response = await fetch("/v1/roon/status", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (body) renderRoon(body);
    } catch (err) { /* the poll retries */ }
  }
  function roonStateLabel(state) {
    var names = {connected: "已连接", pending: "等待启用", connecting: "连接中", failed: "连接失败", disconnected: "未连接"};
    return names[state] || state || "未连接";
  }
  function renderRoon(status) {
    var state = document.getElementById("roon-result");
    if (state) {
      var parts = [roonStateLabel(status.state)];
      if (status.core_name) parts.push(status.core_name);
      if (status.note) parts.push(status.note);
      state.textContent = parts.join(" · ");
    }
    roonZones = status.zones || [];
    var host = document.getElementById("roon-zones");
    if (!host) return;
    if (!roonZones.length) {
      host.innerHTML = status.state === "connected" ? '<p class="meta">Roon 里没有可用的播放区。</p>' : "";
      return;
    }
    host.innerHTML = roonZones.map(function (zone) {
      var playing = zone.now_playing || {};
      var line1 = (playing.three_line && playing.three_line.line1) || (playing.two_line && playing.two_line.line1) || "";
      var line2 = (playing.two_line && playing.two_line.line2) || "";
      var line3 = (playing.three_line && playing.three_line.line3) || "";
      var volume = null, outputID = "";
      (zone.outputs || []).forEach(function (output) {
        if (!volume && output.volume && output.volume.max > 0) { volume = output.volume; outputID = output.output_id; }
      });
      var volumeHTML = "";
      if (volume) {
        var percent = Math.round((volume.value - volume.min) * 100 / Math.max(1, volume.max - volume.min));
        volumeHTML = '<label class="meta">音量 ' + escapeText(String(percent)) + '%<input type="range" min="0" max="100" value="' + percent + '" data-roon-volume="' + escapeText(zone.zone_id) + '" data-roon-output="' + escapeText(outputID) + '"></label>';
      }
      return '<div class="roon-zone"><p><strong>' + escapeText(zone.display_name) + '</strong> <span class="meta">' + escapeText(zone.state || "") + '</span></p>' +
        '<p class="meta">' + escapeText([line1, line2, line3].filter(Boolean).join(" · ")) + '</p>' +
        '<p><button type="button" data-roon-control="previous" data-roon-zone="' + escapeText(zone.zone_id) + '">上一首</button> ' +
        '<button type="button" data-roon-control="playpause" data-roon-zone="' + escapeText(zone.zone_id) + '">播放/暂停</button> ' +
        '<button type="button" data-roon-control="next" data-roon-zone="' + escapeText(zone.zone_id) + '">下一首</button></p>' +
        volumeHTML + '</div>';
    }).join("");
    host.querySelectorAll("[data-roon-control]").forEach(function (button) {
      button.addEventListener("click", function () {
        roonControl(button.getAttribute("data-roon-zone"), button.getAttribute("data-roon-control"), 0, "");
      });
    });
    host.querySelectorAll("[data-roon-volume]").forEach(function (slider) {
      slider.addEventListener("change", function () {
        roonControl(slider.getAttribute("data-roon-volume"), "volume", parseInt(slider.value, 10) || 0, slider.getAttribute("data-roon-output"));
      });
    });
  }
  async function connectRoon() {
    var result = document.getElementById("roon-result");
    if (result) result.textContent = "正在连接 Roon…";
    try {
      var response = await fetch("/v1/roon/connect", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({
        host: document.getElementById("roon-host").value.trim(),
        port: parseInt(document.getElementById("roon-port").value, 10) || 9330
      })});
      var body = await response.json();
      renderRoon(body);
      startRoonPolling();
    } catch (err) {
      if (result) result.textContent = err.message;
    }
  }
  async function disconnectRoon() {
    try {
      var response = await fetch("/v1/roon/disconnect", {method: "POST", headers: {"Content-Type": "application/json"}, body: "{}"});
      var body = await response.json();
      renderRoon(body);
      stopRoonPolling();
    } catch (err) { /* nothing to undo */ }
  }
  async function roonControl(zoneID, control, value, outputID) {
    var result = document.getElementById("roon-result");
    try {
      var response = await fetch("/v1/roon/control", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({
        zone_id: zoneID, output_id: outputID, control: control, value: value
      })});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        if (result) result.textContent = (body && (body.error || body.code)) || "控制没有生效";
        return;
      }
      // The zone state arrives as a subscription push; refresh as well so the
      // line under the buttons is right immediately.
      setTimeout(refreshRoon, 400);
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
      var body = await postSetting("/v1/settings/library", {
        library_dir: document.getElementById("library-dir").value.trim(),
        work_dir: document.getElementById("library-work-dir").value.trim()
      });
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
      await reloadThen("ok", "歌单已保存：" + (body.playlist.name || body.playlist.id) + "，" + body.playlist.track_count + " 首。");
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
    var startedAt = Date.now();
    setStatus("info", "正在读取公开歌单…");
    var ticker = setInterval(function () {
      setStatus("info", "正在读取公开歌单，已 " + Math.round((Date.now() - startedAt) / 1000) + " 秒（四千首的名单通常要 10–20 秒）…");
    }, 1000);
    try {
      var enqueue = document.getElementById("playlist-enqueue");
      var modeBox = document.getElementById("playlist-mode");
      var mode = modeBox ? modeBox.value : "all";
      var payload = {platform: document.getElementById("remote-platform").value, playlist_id: id, save: true, subscribe: true, enqueue: !!(enqueue && enqueue.checked), mode: mode};
      var base = document.getElementById("remote-base");
      if (base && base.value.trim()) payload.base_url = base.value.trim();
      var body = await postSetting("/v1/playlists/fetch", payload);
      var result = document.getElementById("playlist-fetch-result");
      if (result) result.textContent = body.note || "";
      // A job that is created parks in 待核对, which the queue view does not
      // list; switching to tasks is what makes the result visible.
      if (body.job_id && mode === "pick") {
        await reloadThen("ok", (body.note || "") + " 下面是曲目，勾选后点「入库选中」；库里已有的会标出来。");
        await showPlaylistTracks(body.job_id);
        return;
      }
      if (body.job_id) view = "tasks";
      await reloadThen("ok", body.note || "已读取曲目名单。");
    } catch (err) {
      setStatus("fail", "没有读到歌单。" + (err && err.message ? "（" + err.message + "）" : ""));
    } finally {
      clearInterval(ticker);
      setBusy(false);
    }
  }
  // A playlist is a list; the album is the unit the pipeline works in, so the
  // server groups the selection by album and skips what the library has.
  function playlistNeedsWork(row) {
    return typeof row.album_completion_needed === "boolean" ? row.album_completion_needed : !row.present;
  }
  async function showPlaylistTracks(jobID) {
    var box = document.getElementById("playlist-pick");
    if (!box) return;
    var response = await fetch("/v1/playlists/tracks?job=" + encodeURIComponent(jobID), {headers: {Accept: "application/json"}});
    var body = await response.json();
    if (!response.ok || !body || !body.ok) {
      box.innerHTML = '<p class="meta">读不到曲目名单：' + escapeText((body && (body.error || body.code)) || "") + "</p>";
      return;
    }
    var rows = body.tracks || [];
    var present = rows.filter(function (row) { return row.present; }).length;
    var needsWork = rows.filter(playlistNeedsWork).length;
    var missingAlbums = rows.filter(function (row) { return row.present && row.album_completion_needed && !row.album_present; }).length;
    var incompleteAlbums = rows.filter(function (row) { return row.present && row.album_incomplete; }).length;
    var verification = body.verification === "file_stat" ? "精确磁盘校验" : "曲库索引快照";
    var completionUnknown = rows.filter(function (row) { return row.present && row.album_completion_unknown; }).length;
    var html = '<p class="meta">' + escapeText(body.name || "") + "：共 " + rows.length + " 首，曲目已在库 " + present + " 首；核对方式 " + verification + "；按目标专辑补全仍需处理 " + needsWork + " 首" +
      (missingAlbums || incompleteAlbums ? "（目标专辑缺失 " + missingAlbums + "，目标专辑缺轨 " + incompleteAlbums + "）" : "") +
      (completionUnknown ? "；另有 " + completionUnknown + " 首专辑总轨数待后台目录核验，不计为缺轨" : "") + "。" +
      (rows.length > 500 ? " 列表只列出前 500 首，整张入库请用「全部入库」。" : "") + "</p>" +
      '<p><button type="button" id="pick-all">全选</button> <button type="button" id="pick-none">全不选</button> <button type="button" id="pick-apply" class="primary">入库选中</button></p>' +
      '<div class="pick-list">';
    rows.slice(0, 500).forEach(function (row) {
      var needs = playlistNeedsWork(row);
      var state = "";
      if (row.present && row.album_incomplete) state = ' <span class="meta">曲目已有，目标专辑缺轨</span>';
      else if (row.present && needs && !row.album_present) state = ' <span class="meta">曲目已有，目标专辑待补全</span>';
      else if (row.present) state = ' <span class="meta">已有</span>';
      else state = ' <span class="meta">待入库</span>';
      html += '<label class="pick-row"><input type="checkbox" data-pick="' + row.index + '"' + (needs ? " checked" : "") + "> " +
        escapeText(row.artist || "") + " - " + escapeText(row.title || "") +
        (row.album ? ' <span class="meta">' + escapeText(row.album) + "</span>" : "") +
        state + "</label>";
    });
    box.innerHTML = html + "</div>";
    var setAll = function (checked) { box.querySelectorAll("input[data-pick]").forEach(function (input) { input.checked = checked; }); };
    var all = document.getElementById("pick-all");
    if (all) all.addEventListener("click", function () { setAll(true); });
    var none = document.getElementById("pick-none");
    if (none) none.addEventListener("click", function () { setAll(false); });
    var apply = document.getElementById("pick-apply");
    if (apply) apply.addEventListener("click", async function () {
      var indices = [];
      box.querySelectorAll("input[data-pick]:checked").forEach(function (input) { indices.push(parseInt(input.getAttribute("data-pick"), 10)); });
      if (!indices.length) { setStatus("fail", "先勾选要入库的曲目。"); return; }
      var albumBox = document.getElementById("playlist-album");
      apply.disabled = true;
      try {
        var response = await fetch("/v1/playlists/enqueue", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({job_id: jobID, indices: indices, album_completion: !!(albumBox && albumBox.checked)})});
        var result = await response.json();
        if (!response.ok || !result || !result.ok) {
          setStatus("fail", "入库没有开始。" + ((result && (result.error || result.code)) || ""));
          apply.disabled = false;
          return;
        }
        box.innerHTML = "";
        view = "tasks";
        await reloadThen("ok", result.note || "已开始入库。");
      } catch (err) {
        setStatus("fail", err.message);
        apply.disabled = false;
      }
    });
  }
  async function saveSubscription() {
    if (loading) return;
    var playlistID = document.getElementById("subscription-playlist").value;
    if (!playlistID) { setStatus("fail", "先选择已存歌单。"); return; }
    setBusy(true);
    try {
      await postSetting("/v1/subscriptions", {playlist_id: playlistID, interval_hours: parseInt(document.getElementById("subscription-hours").value, 10) || 6, include_existing: document.getElementById("subscription-existing").checked});
      await reloadThen("ok", "已订阅。首次检查前不会创建任务。");
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
      await reloadThen("ok", body.message || "检查完成。");
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
      await reloadThen("ok", paused ? "已暂停订阅。" : "已恢复订阅。");
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
      await reloadThen("ok", "已删除订阅。");
    } catch (err) {
      setStatus("fail", "删除失败。");
    } finally {
      setBusy(false);
    }
  }
  async function preflight() {
    if (loading) return;
    var cookie = document.getElementById("preflight-cookie").value;
    setBusy(true);
    setStatus("info", "正在检查登录");
    try {
      var response = await fetch("/v1/sites/preflight", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({site: document.getElementById("preflight-site").value, cookie: cookie, synthetic: false})});
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
  var libraryRunTimer = 0;
  function libraryByteCount(bytes) {
    var value = Number(bytes || 0);
    if (value >= 1073741824) return (value / 1073741824).toFixed(2) + " GiB";
    if (value >= 1048576) return (value / 1048576).toFixed(1) + " MiB";
    return Math.round(value / 1024) + " KiB";
  }
  function libraryAutomationSummary(report, running) {
    report = report || {};
    var phaseNames = {queued:"排队", reindex:"索引", scrape:"刮削", dedupe:"去重", library_workflows:"升级与补专", done:"完成"};
    var text = running ? ("正在运行 · " + (phaseNames[report.phase] || report.phase || "处理中")) : (report.cancelled ? "上次已停止" : (report.finished ? ("上次完成 · " + report.finished) : "尚未运行"));
    if (report.last_progress) text += " · 最近进度 " + report.last_progress;
    if (!running && report.retry_after_sec) text += " · 约 " + Math.ceil(report.retry_after_sec / 60) + " 分钟后自动继续";
    if (report.current) text += " · " + report.current;
    if (report.phase === "reindex") text += " · 已检查 " + (report.index_checked || 0) + " 项";
    if (report.scan_total) text += " · 扫描 " + (report.scan_checked || 0) + "/" + report.scan_total;
    if (report.scrape_candidates) text += " · 标签 " + (report.scrape_processed || 0) + "/" + report.scrape_candidates;
    if (report.artwork_candidates) text += " · 歌词封面 " + (report.artwork_processed || 0) + "/" + report.artwork_candidates;
    if (report.duplicate_candidates) text += " · 去重 " + (report.duplicate_checked || 0) + "/" + report.duplicate_candidates;
    text += " · 写入 " + (report.scrape_written || 0) + " · 跳过 " + ((report.scrape_skipped || 0) + (report.artwork_skipped || 0)) + " · 失败 " + ((report.scrape_failed || 0) + (report.artwork_failed || 0)) + " · 去重 " + (report.deduplicated || 0) + " · 升级 " + (report.upgrade_jobs || 0) + " · 补专 " + (report.expand_jobs || 0);
    if (report.cache_limit_bytes) text += " · SSD 缓存 " + libraryByteCount(report.cache_used_bytes) + "/" + libraryByteCount(report.cache_limit_bytes) + (report.cache_over_limit ? "（已达上限，新写入暂停）" : "");
    if (report.note) text += " · " + report.note;
    if (report.blocked_by) text += " · 已暂停：" + report.blocked_by + (report.blocked_reason ? "，" + report.blocked_reason : "");
    if (report.errors && report.errors.length) text += " · " + report.errors.slice(-2).join(" | ");
    return text;
  }
  function scheduleLibraryRunRefresh(running) {
    clearTimeout(libraryRunTimer);
    var statusBox = document.getElementById("auto-maintenance-status");
    var runBadge = document.querySelector(".automation-card .run-state");
    document.querySelectorAll("#run-auto-maintenance, #run-library-upgrade-now, #run-library-expand-now, #run-library-scrape-now").forEach(function (button) { button.disabled = !!running; });
    var stop = document.getElementById("stop-auto-maintenance");
    if (stop) stop.disabled = !running;
    if (!statusBox) return;
    libraryRunTimer = setTimeout(async function () {
      if (view !== "library") return;
      try {
        var response = await fetch("/v1/library/automation", {headers:{Accept:"application/json"}});
        var body = await response.json();
        if (!response.ok || !body || !body.ok) throw new Error("status");
        libraryAutomation = body;
        statusBox.textContent = libraryAutomationSummary(body.report || {}, !!body.running);
        statusBox.classList.toggle("running", !!body.running);
        if (runBadge) {
          runBadge.textContent = body.running ? "运行中" : "空闲";
          runBadge.classList.toggle("running", !!body.running);
        }
        scheduleLibraryRunRefresh(!!body.running);
      } catch (err) {
        libraryRunTimer = setTimeout(function () { scheduleLibraryRunRefresh(true); }, 5000);
      }
    }, running ? 3000 : 10000);
  }
  function renderLibrary() {
    var auto = libraryAutomation && libraryAutomation.settings ? libraryAutomation.settings : {enabled:true, scrape:true, dedupe:true, upgrade:true, expand:true, interval_hours:6};
    var autoReport = libraryAutomation && libraryAutomation.report ? libraryAutomation.report : {};
    var autoRunning = !!(libraryAutomation && libraryAutomation.running);
    var autoText = libraryAutomationSummary(autoReport, autoRunning);
    var automationCard = '<article class="automation-card"><div class="section-heading"><div><h2>自动维护</h2><p class="section-copy">索引优先，未变化的音频不重读；扫描、哈希、补图与歌单校验各使用一个低并发通道并主动节流，避免播放时出现整库突发读写。</p></div><span class="run-state ' + (autoRunning ? "running" : "") + '">' + (autoRunning ? "运行中" : "空闲") + '</span></div>' +
      '<div class="maintenance-status ' + (autoRunning ? "running" : "") + '" id="auto-maintenance-status" role="status" aria-live="polite">' + escapeText(autoText) + '</div>' +
      '<div class="option-grid">' +
      '<label><input type="checkbox" id="auto-maintenance-enabled"' + (auto.enabled ? " checked" : "") + '> 持续自动维护</label>' +
      '<label><input type="checkbox" id="auto-maintenance-scrape"' + (auto.scrape ? " checked" : "") + '> 刮削补齐</label>' +
      '<label><input type="checkbox" id="auto-maintenance-dedupe"' + (auto.dedupe ? " checked" : "") + '> 最佳音质去重</label>' +
      '<label><input type="checkbox" id="auto-maintenance-upgrade"' + (auto.upgrade ? " checked" : "") + '> 音质升级</label>' +
      '<label><input type="checkbox" id="auto-maintenance-expand"' + (auto.expand ? " checked" : "") + '> 单曲补整专</label>' +
      '<label>周期（小时）<input type="text" id="auto-maintenance-hours" value="' + escapeText(auto.interval_hours || 6) + '"></label></div>' +
      '<div class="action-row"><button type="button" id="save-auto-maintenance" class="primary">保存设置</button><button type="button" id="run-auto-maintenance">立即维护全库</button><button type="button" id="run-library-upgrade-now">一键升级全库音质</button><button type="button" id="run-library-expand-now">一键补齐单曲所属专辑</button><button type="button" id="run-library-scrape-now">一键全库刮削（标签·歌手·专辑·封面·歌词）</button><button type="button" id="stop-auto-maintenance" class="quiet">停止</button></div><p id="auto-maintenance-result" class="meta"></p></article>';
    var duplicateHtml = "";
    if (duplicateRows.length) {
      duplicateRows.forEach(function (group) {
        duplicateHtml += '<p>' + escapeText((group.artists || []).join("、") + " / " + (group.album || "")) + " · " + escapeText(group.state || "") + " · " + escapeText(group.reason || "") +
          (group.state === "ready" ? ' <button type="button" data-merge="' + escapeText(group.group_id) + '">合并重复项</button>' : "") + "</p>";
      });
    } else {
      duplicateHtml = '<p class="meta">尚未扫描，或没有可合并的重复项。</p>';
    }
    var html = '<div class="cards">' + automationCard + '<article><div class="section-heading"><div><h2>本地资料库</h2><p class="section-copy">筛选、全选并启动补缺、音质升级或整专补全。</p></div></div>' +
      '<div class="action-row compact"><button type="button" id="reindex-library" class="primary">重新索引</button><button type="button" id="list-gaps">查看缺轨</button></div>' +
      '<div class="selection-toolbar"><div class="action-row compact"><button type="button" id="library-select-page">选择本页</button><button type="button" id="library-select-all">全选全部</button><button type="button" id="library-clear-selection" class="quiet">清空</button></div><span id="library-selection-label" class="selection-count">已选 0 首</span></div>' +
      '<div class="pagination"><button type="button" id="library-page-prev">上一页</button><button type="button" id="library-page-next">下一页</button><span id="library-page-label" class="meta"></span></div>' +
      '<div id="library-file" class="file-picker"></div>' +
      '<div class="action-row"><button type="button" id="fill-gaps">所选补缺</button><button type="button" id="upgrade-album">所选升级音质</button><button type="button" id="expand-album">所选扩成整专</button></div>' +
      '<p id="library-ops-result" class="meta"></p></article>' +
      '<article><h2>标签、封面和歌词</h2><p class="meta">目录确认后，曲名、艺人、专辑艺人、专辑、日期、音轨号、碟号和分类（GENRE）按目录补写，歌词和封面同一次写入。文件里已有且与目录不同的值默认保留，勾选覆盖才会改。封面写入 FLAC 内嵌图片；按设置可以同时保留同名 jpg。写之前会备份，任何一步失败都恢复原文件。目录地址只接受本机；歌词和封面留空时用设置里的服务（须为 HTTPS）。</p>' +
      '<label>文件路径<input type="text" id="meta-path" placeholder="库内 .flac"></label>' +
      '<label>艺人<input type="text" id="meta-artist"></label><label>专辑<input type="text" id="meta-album"></label><label>曲名<input type="text" id="meta-title"></label>' +
      '<label>目录地址<input type="text" id="meta-catalog" placeholder="本机 MusicBrainz"></label>' +
      '<label>歌词地址<input type="text" id="meta-lyrics" placeholder="留空用设置里的歌词服务；填了只覆盖这一次"></label>' +
      '<label>封面地址<input type="text" id="meta-cover" placeholder="留空用设置里的封面服务；填了只覆盖这一次"></label>' +
      '<label>封面写入<select id="meta-cover-mode"><option value="sidecar">内嵌并保留同名 jpg</option><option value="embedded">只写入 FLAC 内嵌图片</option></select></label>' +
      '<label><input type="checkbox" id="meta-overwrite"> 覆盖与目录不同的字段（含分类）</label>' +
      '<p><button type="button" id="complete-metadata">补齐标签封面歌词</button></p><p id="metadata-result" class="meta"></p>' +
      '<p class="meta">目录没有收录这张专辑时，可以从网易云 / QQ / 酷狗 / 酷我查候选，再手动填进上面的艺人、专辑、曲名。</p>' +
      '<p><button type="button" id="lookup-sources">查其它来源</button></p><div id="sources-result" class="meta"></div>' +
      '<p><button type="button" id="backfill-lrc">补写同名 lrc</button> <button type="button" id="backfill-lrc-preview">先看会改多少</button></p>' +
      '<p id="backfill-lrc-result" class="meta">扫描整个音乐库：已有歌词标签但没有 .lrc 的补写，旧命名（xxx.flac.lrc）改成 xxx.lrc，已存在的同名 .lrc 不动。</p>' +
      '<p class="meta">按专辑查询目录并补写歌名、歌手、专辑、音轨号和分类；全库模式同时补齐缺失封面与歌词。所选模式单次最多 200 首，含歌词封面时最多 30 首；全选全部使用后台任务。</p>' +
      '<label><input type="checkbox" id="scrape-artwork"> 同时补歌词与封面</label>' +
      '<div class="action-row"><button type="button" id="scrape-preview">预演所选</button><button type="button" id="scrape-apply" class="primary">刮削所选</button><button type="button" id="scrape-whole-library">全库补齐标签·封面·歌词</button></div></article>' +
      '<article><h2>合辑</h2><p class="meta">预览不会移动文件。合并只把名称里带“合辑”或 Various Artists 的文件夹移进音乐库的 Compilations。</p>' +
      '<p><button type="button" id="preview-compilations">预览合辑</button> <button type="button" id="merge-compilations">合并合辑</button></p><p id="compilation-result" class="meta"></p></article>' +
      '<article><h2>艺人目录与 NCM</h2><p class="meta">艺人目录把专辑文件夹整理成“艺人/专辑”。NCM 解密不改源文件。</p>' +
      '<p><button type="button" id="plan-artist-folders">查看艺人目录</button> <button type="button" id="apply-artist-folders" class="primary">一键补全歌手目录</button></p>' +
      '<label>NCM 路径<input type="text" id="ncm-source" placeholder="库内 .ncm 路径"></label><p><button type="button" id="decrypt-ncm">解密 NCM</button></p><p id="library-tools-result" class="meta"></p></article>' +
      '<article><h2>重复歌曲</h2><p class="meta">同一专辑的同一录音按真实编码、位深、采样率和码率比较，保留可完整解码的最高音质版本；不同版本、母带或声道不会合并。旧文件进入可恢复隔离区。</p>' +
      '<p class="meta">后台扫描优先读取本地指纹索引，只对缺失项做限速音频校验；暂停调度会停止正在运行的扫描并保存已完成进度。</p><div class="maintenance-status ' + (duplicateScanStatus && duplicateScanStatus.running ? "running" : "") + '" id="duplicate-scan-status" role="status" aria-live="polite">' + escapeText(duplicateScanLabel()) + '</div><p><button type="button" id="scan-duplicates" class="primary"' + (duplicateScanStatus && duplicateScanStatus.running ? " disabled" : "") + '>低频扫描重复</button><button type="button" id="stop-duplicate-scan" class="quiet"' + (duplicateScanStatus && duplicateScanStatus.running ? "" : " disabled") + '>停止扫描</button></p>' + duplicateHtml +
      '<label>恢复编号<input type="text" id="restore-merge-id" placeholder="合并后返回的编号"></label><p><button type="button" id="restore-duplicates">恢复这次合并</button></p></article>' +
      guardCard() +
      '<article><form id="transcode-form"><h2>WAV 无损转 FLAC</h2>' +
      '<p class="meta">整数 PCM（16/24/32 位、多声道、可扩展格式）转成 FLAC，样本逐个比对并写入 MD5。浮点和压缩 WAV 会拒绝，不会降级。标签从 LIST/INFO、ID3 和 BWF 迁入，封面写入内嵌图片。没有标签时只按文件名和目录补显示名，不把它当成已确认的发行。</p>' +
      '<label><input type="checkbox" id="wav-auto" checked> 下载进入待入库或重新索引时，先转成 FLAC 再识别入库（默认开启）</label>' +
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
    var saveAuto = document.getElementById("save-auto-maintenance");
    if (saveAuto) saveAuto.addEventListener("click", saveLibraryAutomation);
    var runAuto = document.getElementById("run-auto-maintenance");
    if (runAuto) runAuto.addEventListener("click", runLibraryAutomationNow);
    var runScrape = document.getElementById("run-library-scrape-now");
    if (runScrape) runScrape.addEventListener("click", runWholeLibraryScrape);
    var runUpgrade = document.getElementById("run-library-upgrade-now");
    if (runUpgrade) runUpgrade.addEventListener("click", runWholeLibraryUpgrade);
    var runExpand = document.getElementById("run-library-expand-now");
    if (runExpand) runExpand.addEventListener("click", runWholeLibraryExpansion);
    var wholeScrape = document.getElementById("scrape-whole-library");
    if (wholeScrape) wholeScrape.addEventListener("click", runWholeLibraryScrape);
    var stopAuto = document.getElementById("stop-auto-maintenance");
    if (stopAuto) stopAuto.addEventListener("click", stopLibraryAutomationNow);
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
    var selectPage = document.getElementById("library-select-page");
    if (selectPage) selectPage.addEventListener("click", function () { selectLibraryPage(true); });
    var selectAll = document.getElementById("library-select-all");
    if (selectAll) selectAll.addEventListener("click", function () { setLibrarySelectionAll(true); });
    var clearSelection = document.getElementById("library-clear-selection");
    if (clearSelection) clearSelection.addEventListener("click", function () { setLibrarySelectionAll(false); });
    var metadata = document.getElementById("complete-metadata");
    if (metadata) metadata.addEventListener("click", completeMetadata);
    var backfillLrc = document.getElementById("backfill-lrc");
    if (backfillLrc) backfillLrc.addEventListener("click", function () { backfillLyricsSidecars(false); });
    var backfillPreview = document.getElementById("backfill-lrc-preview");
    if (backfillPreview) backfillPreview.addEventListener("click", function () { backfillLyricsSidecars(true); });
    var lookup = document.getElementById("lookup-sources");
    if (lookup) lookup.addEventListener("click", lookupSources);
    var scrapePreview = document.getElementById("scrape-preview");
    if (scrapePreview) scrapePreview.addEventListener("click", function () { scrapeSelected(true); });
    var scrapeApply = document.getElementById("scrape-apply");
    if (scrapeApply) scrapeApply.addEventListener("click", function () { scrapeSelected(false); });
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
    var stopDuplicateScan = document.getElementById("stop-duplicate-scan");
    if (stopDuplicateScan) stopDuplicateScan.addEventListener("click", async function () {
      stopDuplicateScan.disabled = true;
      setStatus("info", "正在停止低频重复扫描");
      try {
        var response = await fetch("/v1/library/duplicates/stop", {method: "POST", headers: {"Content-Type": "application/json", Accept: "application/json"}, body: "{}"});
        var body = await response.json();
        if (!response.ok || !body || !body.ok) throw new Error(body && body.code ? body.code : "停止失败");
        duplicateScanStatus = body.status || duplicateScanStatus;
        setStatus("info", "已发送停止请求；扫描会在当前读块结束后退出。");
      } catch (err) {
        stopDuplicateScan.disabled = false;
        setStatus("fail", "停止扫描失败。");
      }
    });
    var scan = document.getElementById("scan-duplicates");
    if (scan) scan.addEventListener("click", scanDuplicates);
    var restore = document.getElementById("restore-duplicates");
    if (restore) restore.addEventListener("click", restoreDuplicates);
    document.querySelectorAll("[data-merge]").forEach(function (button) {
      button.addEventListener("click", function () { mergeDuplicates(button.getAttribute("data-merge")); });
    });
    var guardSave = document.getElementById("guard-save");
    if (guardSave) guardSave.addEventListener("click", function () {
      var dir = document.getElementById("guard-dir").value.trim();
      if (!dir) { document.getElementById("guard-result").textContent = "先填专辑目录。"; return; }
      saveGuard(dir, selectedGuardFields(), document.getElementById("guard-suppress").checked);
    });
    var guardClear = document.getElementById("guard-clear");
    if (guardClear) guardClear.addEventListener("click", function () {
      var dir = document.getElementById("guard-dir").value.trim();
      if (!dir) { document.getElementById("guard-result").textContent = "先填专辑目录。"; return; }
      saveGuard(dir, [], false);
    });
    refreshGuards();
    scheduleLibraryRunRefresh(!!(libraryAutomation && libraryAutomation.running));
  }
  // Guards are per album directory: the field list comes from the server so
  // the page never invents a name the writer would not honour.
  var guardMeta = {fields: []};
  var guardRows = [];
  function guardCard() {
    return '<article><h2>字段锁与去重抑制</h2><p class="meta">按专辑目录保存。字段锁让自动写入（补齐标签、批量刮削、合并补标签）跳过这些字段，即使勾选覆盖也不改；抑制让这张专辑不参与重复扫描与合并。目录改名或移动后锁不再生效。</p>' +
      '<label>专辑目录<input type="text" id="guard-dir" placeholder="库内专辑文件夹，如 Artist/Album"></label>' +
      '<fieldset><legend>锁定的字段</legend><span id="guard-fields"></span></fieldset>' +
      '<label><input type="checkbox" id="guard-suppress"> 不参与重复扫描与合并</label>' +
      '<p><button type="button" id="guard-save" class="primary">保存</button> <button type="button" id="guard-clear">清除这张专辑</button></p>' +
      '<p id="guard-result" class="meta"></p><p id="guard-list" class="meta"></p></article>';
  }
  async function refreshGuards() {
    var target = document.getElementById("guard-list");
    try {
      var response = await fetch("/v1/library/guards", {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!body || !body.ok) { if (target) target.textContent = "没有读到字段锁。"; return; }
      guardMeta.fields = body.fields || [];
      guardRows = body.albums || [];
      var fields = document.getElementById("guard-fields");
      if (fields) {
        fields.innerHTML = guardMeta.fields.map(function (field) {
          return '<label><input type="checkbox" data-guard-field="' + escapeText(field) + '"> ' + escapeText(fieldLabel(field)) + "</label>";
        }).join("");
      }
      if (target) {
        target.innerHTML = guardRows.length
          ? "已保存：" + guardRows.map(function (row) {
              var parts = [];
              if ((row.fields || []).length) parts.push((row.fields || []).join("、"));
              if (row.suppressed) parts.push("不参与合并");
              return '<a href="#" data-guard-dir="' + escapeText(row.dir) + '">' + escapeText(row.dir) + "</a>（" + escapeText(parts.join("；")) + "）";
            }).join("<br>")
          : "还没有保存的字段锁。";
        // Clicking a saved album loads its switches into the form, which is
        // the only way to edit one without retyping the path.
        target.querySelectorAll("[data-guard-dir]").forEach(function (link) {
          link.addEventListener("click", function (event) {
            event.preventDefault();
            fillGuard(link.getAttribute("data-guard-dir"));
          });
        });
      }
    } catch (err) {
      if (target) target.textContent = "没有读到字段锁。";
    }
  }
  function fieldLabel(field) {
    var names = {title: "曲名", artist: "艺人", album: "专辑", albumartist: "专辑艺人", date: "日期", genre: "分类", tracknumber: "音轨号", discnumber: "碟号", lyrics: "歌词", composer: "作曲"};
    return names[field] || field;
  }
  async function saveGuard(dir, fields, suppressed) {
    var result = document.getElementById("guard-result");
    try {
      var response = await fetch("/v1/library/guards", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({dir: dir, fields: fields, suppressed: suppressed})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        if (result) result.textContent = (body && (body.error || body.code)) || "没有保存。";
        return;
      }
      if (result) result.textContent = (fields.length || suppressed) ? "已保存：" + body.dir : "已清除：" + body.dir;
      refreshGuards();
    } catch (err) {
      if (result) result.textContent = "保存中断。";
    }
  }
  function fillGuard(dir) {
    var input = document.getElementById("guard-dir");
    if (input) input.value = dir;
    var row = null;
    guardRows.forEach(function (item) { if (item.dir === dir) row = item; });
    document.querySelectorAll("[data-guard-field]").forEach(function (box) {
      box.checked = !!row && (row.fields || []).indexOf(box.getAttribute("data-guard-field")) >= 0;
    });
    var suppress = document.getElementById("guard-suppress");
    if (suppress) suppress.checked = !!row && !!row.suppressed;
  }
  function selectedGuardFields() {
    var out = [];
    document.querySelectorAll("[data-guard-field]").forEach(function (box) {
      if (box.checked) out.push(box.getAttribute("data-guard-field"));
    });
    return out;
  }
  // lookupSourcesAsks the streaming catalogues for candidates. They are
  // labelled as candidates: filling the form with them is the user's call, and
  // the tag writer still goes through the normal completion flow.
  // The walk takes minutes on a real library, so the page starts it and
  // polls: a request that never answers looks like a hang.
  function lyricsReportLine(report, running, dryRun) {
    var parts = [];
    parts.push((running ? "进行中" : (dryRun ? "预演完成" : "已完成")) + "：扫描 " + (report.scanned || 0) + " 首");
    if (report.renamed) parts.push("改名 " + report.renamed);
    if (report.written) parts.push("补写 " + report.written);
    if (report.kept) parts.push("已有 " + report.kept);
    if (report.missing) parts.push("无歌词 " + report.missing);
    if (report.failed && report.failed.length) parts.push("失败 " + report.failed.length);
    if (report.error) parts.push(report.error);
    return parts.join("；") + "。";
  }
  async function backfillLyricsSidecars(dryRun) {
    var result = document.getElementById("backfill-lrc-result");
    try {
      var response = await fetch("/v1/library/lyrics/backfill", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({dry_run: dryRun})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        if (result) result.textContent = (body && (body.error || body.code)) || "没有完成。";
        return;
      }
      if (result) result.textContent = lyricsReportLine(body.report || {}, true, dryRun);
      var poll = setInterval(async function () {
        try {
          var status = await (await fetch("/v1/library/lyrics/backfill", {headers: {Accept: "application/json"}})).json();
          if (result) result.textContent = lyricsReportLine(status.report || {}, status.running, dryRun);
          if (!status.running) clearInterval(poll);
        } catch (err) {
          clearInterval(poll);
          if (result) result.textContent = err.message;
        }
      }, 2000);
    } catch (err) {
      if (result) result.textContent = err.message;
    }
  }
  async function lookupSources() {
    if (loading) return;
    var out = document.getElementById("sources-result");
    var title = document.getElementById("meta-title").value.trim();
    var artist = document.getElementById("meta-artist").value.trim();
    if (!title && !artist) { if (out) out.textContent = "先填曲名或艺人。"; return; }
    if (out) out.textContent = "正在查网易云 / QQ / 酷狗 / 酷我…";
    setBusy(true);
    try {
      var response = await fetch("/v1/sources/lookup", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({title: title, artist: artist, query: (title + " " + artist).trim()})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) { if (out) out.textContent = (body && (body.error || body.code)) || "查询失败。"; return; }
      var rows = body.candidates || [];
      if (!rows.length) { if (out) out.textContent = "这些来源也没有找到。" + (body.note ? " " + body.note : ""); return; }
      out.innerHTML = rows.map(function (row, index) {
        return '<p>' + escapeText(row.source) + '：' + escapeText(row.title) + " — " + escapeText(row.artist) + (row.album ? " · " + escapeText(row.album) : "") + (row.exact ? "（对得上）" : "（候选）") + ' <button type="button" data-use-source="' + index + '">填进表单</button></p>';
      }).join("") + '<p class="meta">' + escapeText(body.note || "") + "</p>";
      out.querySelectorAll("[data-use-source]").forEach(function (button) {
        button.addEventListener("click", function () {
          var row = rows[parseInt(button.getAttribute("data-use-source"), 10)];
          if (!row) return;
          document.getElementById("meta-title").value = row.title || "";
          document.getElementById("meta-artist").value = row.artist || "";
          document.getElementById("meta-album").value = row.album || "";
        });
      });
    } catch (err) {
      if (out) out.textContent = "查询中断。";
    } finally {
      setBusy(false);
    }
  }
  function selectedTrackPaths() {
    return Object.keys(selectedTracks).filter(function (path) { return selectedTracks[path]; });
  }
  function updateLibrarySelectionLabel() {
    var label = document.getElementById("library-selection-label");
    if (!label) return;
    var count = librarySelectAll ? libraryFileTotal : selectedTrackPaths().length;
    label.textContent = (librarySelectAll ? "已全选 " : "已选 ") + count + " 首";
    label.classList.toggle("all", librarySelectAll);
  }
  function selectLibraryPage(checked) {
    librarySelectAll = false;
    libraryPageFiles.forEach(function (path) { selectedTracks[path] = checked; });
    document.querySelectorAll("#library-file .library-track").forEach(function (box) { box.checked = checked; });
    updateLibrarySelectionLabel();
  }
  function setLibrarySelectionAll(selected) {
    librarySelectAll = !!selected;
    selectedTracks = {};
    document.querySelectorAll("#library-file .library-track").forEach(function (box) { box.checked = librarySelectAll; });
    updateLibrarySelectionLabel();
  }
  function confirmTracks(scene, label) {
    var paths = selectedTrackPaths();
    var result = document.getElementById("library-ops-result");
    if (librarySelectAll) {
      if (scene === "upgrade") {
        if (result) result.textContent = "正在为全库有损曲目创建最佳音质升级任务。";
        runWholeLibraryUpgrade();
      } else if (result) {
        result.textContent = "全选全部只用于音质升级和重新刮削；补缺与扩专需要按专辑选择。";
      }
      return;
    }
    if (!paths.length) {
      if (result) result.textContent = "先选择曲目。下载仍受令牌规则约束，这一步不会入库。";
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
      libraryFileTotal = total;
      libraryPageFiles = (body.files || []).slice();
      if (libraryFileOffset > 0 && libraryFileOffset >= total) {
        libraryFileOffset = Math.max(0, total - 20);
      }
      var html = "";
      libraryPageFiles.forEach(function (file) {
        html += '<label class="file-row"><input type="checkbox" class="library-track" value="' + escapeText(file) + '"' + ((librarySelectAll || selectedTracks[file]) ? " checked" : "") + "><span>" + escapeText(file) + "</span></label>";
      });
      list.innerHTML = html || '<p class="meta">这一页没有音频文件。</p>';
      if (label) label.textContent = total ? ("第 " + (libraryFileOffset + 1) + "–" + (libraryFileOffset + libraryPageFiles.length) + " 首，共 " + total + " 首") : "0 首";
      list.querySelectorAll(".library-track").forEach(function (box) {
        box.addEventListener("change", function () {
          librarySelectAll = false;
          selectedTracks[box.value] = box.checked;
          updateLibrarySelectionLabel();
        });
      });
      updateLibrarySelectionLabel();
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
      cover_mode: document.getElementById("meta-cover-mode").value,
      overwrite: document.getElementById("meta-overwrite").checked
    }, "metadata-result");
  }
  async function saveLibraryAutomation() {
    var result = document.getElementById("auto-maintenance-result");
    try {
      var response = await fetch("/v1/library/automation", {method:"POST", headers:{"Content-Type":"application/json"}, body:JSON.stringify({
        enabled: document.getElementById("auto-maintenance-enabled").checked,
        scrape: document.getElementById("auto-maintenance-scrape").checked,
        dedupe: document.getElementById("auto-maintenance-dedupe").checked,
        upgrade: document.getElementById("auto-maintenance-upgrade").checked,
        expand: document.getElementById("auto-maintenance-expand").checked,
        interval_hours: parseInt(document.getElementById("auto-maintenance-hours").value, 10) || 6
      })});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有保存");
      if (result) result.textContent = "自动维护设置已保存。";
      await load(true);
    } catch (err) {
      if (result) result.textContent = "保存失败：" + err.message;
    }
  }
  async function runLibraryAutomationNow() {
    var result = document.getElementById("auto-maintenance-result");
    try {
      var response = await fetch("/v1/library/automation/run", {method:"POST", headers:{"Content-Type":"application/json"}, body:"{}"});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有启动");
      if (result) result.textContent = body.started ? "整库自动维护已启动，可在页面查看进度。" : "已有一轮自动维护正在运行。";
      await load(true);
    } catch (err) {
      if (result) result.textContent = "启动失败：" + err.message;
    }
  }
  async function runWholeLibraryScrape() {
    var result = document.getElementById("auto-maintenance-result");
    setStatus("info", "正在启动异步整库刮削…");
    try {
      var response = await fetch("/v1/library/scrape/whole", {method:"POST", headers:{"Content-Type":"application/json", Accept:"application/json"}, body:"{}"});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有启动");
      var message = body.started ? "整库刮削已启动；将并行扫描全部格式并持续补齐标签、歌词和封面。" : "已有维护正在运行，未重复启动。";
      if (result) result.textContent = message;
      setStatus(body.started ? "ok" : "review", message);
      await load(true);
    } catch (err) {
      if (result) result.textContent = "启动失败：" + err.message;
      setStatus("fail", "整库刮削没有启动。" + err.message);
    }
  }
  async function runWholeLibraryUpgrade() {
    var result = document.getElementById("auto-maintenance-result");
    setStatus("info", "正在启动全库音质升级…");
    try {
      var response = await fetch("/v1/library/upgrade/whole", {method:"POST", headers:{"Content-Type":"application/json", Accept:"application/json"}, body:"{}"});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有启动");
      var message = body.started ? "全库音质升级已启动；会为有损曲目逐个创建最佳音质检索任务。" : "已有维护正在运行，未重复启动。";
      if (result) result.textContent = message;
      setStatus(body.started ? "ok" : "review", message);
      await load(true);
    } catch (err) {
      if (result) result.textContent = "启动失败：" + err.message;
      setStatus("fail", "全库音质升级没有启动。" + err.message);
    }
  }
  async function runWholeLibraryExpansion() {
    var result = document.getElementById("auto-maintenance-result");
    setStatus("info", "正在启动全库单曲补整专…");
    try {
      var response = await fetch("/v1/library/expand/whole", {method:"POST", headers:{"Content-Type":"application/json", Accept:"application/json"}, body:"{}"});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有启动");
      var message = body.started ? "全库整专补全已启动；会先核对目录是否确认多曲目，再只排入确实缺轨的专辑。" : "已有维护正在运行，未重复启动。";
      if (result) result.textContent = message;
      setStatus(body.started ? "ok" : "review", message);
      await load(true);
    } catch (err) {
      if (result) result.textContent = "启动失败：" + err.message;
      setStatus("fail", "全库整专补全没有启动。" + err.message);
    }
  }
  async function stopLibraryAutomationNow() {
    var result = document.getElementById("auto-maintenance-result");
    setStatus("info", "正在安全停止当前整库维护…");
    try {
      var response = await fetch("/v1/library/automation/stop", {method:"POST", headers:{"Content-Type":"application/json", Accept:"application/json"}, body:"{}"});
      var body = await response.json();
      if (!response.ok || !body.ok) throw new Error(body.error || body.code || "没有停止");
      var message = body.stopping ? "停止请求已发送；当前网络请求或文件操作结束后退出，已完成结果会保留。" : "当前没有正在运行的整库维护。";
      if (result) result.textContent = message;
      setStatus("review", message);
      await load(true);
    } catch (err) {
      if (result) result.textContent = "停止失败：" + err.message;
      setStatus("fail", "停止请求失败。" + err.message);
    }
  }
  async function scrapeSelected(dryRun) {
    if (loading) return;
    if (librarySelectAll) {
      if (dryRun) {
        setStatus("fail", "全选全部不能逐首预演；请选择本页后预演，或直接启动后台重刮全库。");
        return;
      }
      await runWholeLibraryScrape();
      return;
    }
    var paths = selectedTrackPaths();
    if (!paths.length) { setStatus("fail", "先选择要刮削的曲目。"); return; }
    setBusy(true);
    setStatus("info", dryRun ? "正在预演批量刮削" : "正在批量刮削");
    try {
      var artwork = document.getElementById("scrape-artwork");
      var response = await fetch("/v1/library/scrape", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({
        paths: paths,
        catalog_base_url: document.getElementById("meta-catalog").value.trim(),
        overwrite: document.getElementById("meta-overwrite").checked,
        dry_run: dryRun,
        with_artwork: !!(artwork && artwork.checked)
      })});
      var body = await response.json();
      if (!response.ok || !body || body.ok === false) {
        setStatus("fail", (body && (body.error || body.code)) || "批量刮削没有完成。");
        return;
      }
      var text = (dryRun ? ("预演：将写入 " + body.planned + " 首") : ("已写入 " + body.written + " 首"))
        + "；未变化 " + body.unchanged + "；跳过 " + body.skipped + "；失败 " + body.failed;
      if (body.genre_written) text += "；其中写入分类 " + body.genre_written + " 首";
      if (body.lyrics_written) text += "；歌词 " + body.lyrics_written + " 首";
      if (body.cover_written) text += "；封面 " + body.cover_written + " 首";
      setStatus("ok", text + "。" + (body.note || ""));
    } catch (err) {
      setStatus("fail", "批量刮削中断。");
    } finally {
      setBusy(false);
    }
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
    setStatus("info", "正在启动低频重复扫描");
    try {
      var response = await fetch("/v1/library/duplicates/start", {method: "POST", headers: {"Content-Type": "application/json", Accept: "application/json"}, body: "{}"});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        if (body && body.code === "LIBRARY_SCAN_CANCELED") {
          setStatus("info", body.error || "重复扫描已停止；检查点已保存。");
          return;
        }
        setStatus("fail", "扫描启动失败。" + (body && body.code ? "（" + body.code + "）" : ""));
        return;
      }
      duplicateScanStatus = body.status || duplicateScanStatus;
      if (body.result) {
        duplicateRows = body.result.groups || [];
        body = body.result;
      }
      while (duplicateScanStatus && duplicateScanStatus.running) {
        var duplicateLabel = document.getElementById("duplicate-scan-status");
        if (duplicateLabel) duplicateLabel.textContent = duplicateScanLabel();
        await new Promise(function (resolve) { setTimeout(resolve, 1000); });
        var statusResponse = await fetch("/v1/library/duplicates/status?include=result", {headers: {Accept: "application/json"}});
        var statusBody = await statusResponse.json();
        if (!statusResponse.ok || !statusBody || !statusBody.ok) throw new Error(statusBody && statusBody.code ? statusBody.code : "扫描状态不可用");
        duplicateScanStatus = statusBody.status || duplicateScanStatus;
        if (!duplicateScanStatus.running) {
          if (statusBody.result) {
            duplicateRows = statusBody.result.groups || [];
            body = statusBody.result;
          }
          break;
        }
      }
      if (duplicateScanStatus && duplicateScanStatus.cancelled) {
        setStatus("info", duplicateScanStatus.message || "重复扫描已停止；检查点已保存。");
        renderLibrary();
        return;
      }
      if (duplicateScanStatus && duplicateScanStatus.error) {
        setStatus("fail", "扫描失败。" + duplicateScanStatus.error);
        renderLibrary();
        return;
      }
      duplicateRows = (body && body.groups) || duplicateRows;
      if (!body || !Array.isArray(body.groups)) throw new Error("扫描结果缺失");
      if (body.truncated) {
        setStatus("info", "已显示前 " + duplicateRows.length + " 组，仍有候选未展开。自动维护会继续按最佳音质分批处理；同名未确认的不会合并。");
      } else {
        setStatus("ok", "扫描完成，" + duplicateRows.length + " 组。同名未确认的不会合并。");
      }
      renderLibrary();
    } catch (err) {
      setStatus("fail", "扫描状态连接中断；后台任务可能仍在运行，可重新打开库页查看状态。");
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
    if (!window.confirm("只撤掉迁入的曲目索引、待核对任务和令牌记录。音乐文件和旧库归档会保留。")) return;
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
      var summary = body.summary || job;
      var actionID = summary.job_id || job.job_id;
      var reviewID = summary.stage === "needs_review" ? actionID : job.job_id;
      list.hidden = true;
      note.hidden = true;
      detail.hidden = false;
      document.getElementById("detail-title").textContent = job.label || "未命名";
      detailFields.replaceChildren();
      field("整体状态", executionLabel(summary.execution_state, summary.paused, summary.retry_after_ns) || "未知");
      if (actionID && actionID !== job.job_id) field("当前环节", actionID);
      field("进度", body.progress);
      field("说明", body.headline);
      field("下载", body.downloaded ? "已下载" : "未下载");
      field("入库", body.library_published ? "已入库" : "未入库");
      if (body.result) {
        var failure = body.result;
        var diagnostic = [failure.error_class, failure.phase, failure.message].filter(Boolean).join(" · ");
        field("失败诊断", diagnostic || failure.state || "未返回详细原因");
      } else if (summary.error_class || summary.message) {
        field("失败诊断", [summary.error_class, summary.message].filter(Boolean).join(" · "));
      }
      currentJob = job;
      currentActionJobID = actionID || job.job_id;
      currentActionStage = summary.stage || "";
      currentReviewJobID = reviewID;
      showActions(summary, job);
      if (summary.stage === "needs_review") openReview(currentReviewJobID);
      else document.getElementById("review-box").hidden = true;
      setStatus("", "");
    } catch (err) {
      setStatus("fail", "读取中断。");
    } finally {
      setBusy(false);
    }
  }
  function showActions(state, root) {
    state = state || root;
    root = root || state;
    var terminal = state.stage === "completed" || state.stage === "failed" || state.stage === "cancelled" || state.stage === "skipped";
    var needsReview = state.stage === "needs_review";
    // The aggregate state may come from a descendant task. It is the action
    // target, while the root remains the stable card and title.
    document.getElementById("pause-job").hidden = terminal || needsReview || !!state.paused;
    document.getElementById("resume-job").hidden = terminal || needsReview || !state.paused;
    document.getElementById("cancel-job").hidden = terminal || needsReview || !!state.cancel_requested;
    document.getElementById("retry-job").hidden = state.stage !== "failed";
    document.getElementById("torrent-actions").hidden = !(state.stage === "downloading" || state.stage === "failed");
  }
  // torrentControl talks to the client, not just to the workflow: pausing the
  // job alone leaves the torrent running, and deleting keeps the files.
  async function torrentControl(action) {
    if (!currentJob || loading) return;
    if (action === "delete" && !window.confirm("从 qBittorrent 移除这个种子吗？已下载的文件会保留。")) return;
    setBusy(true);
    setStatus("info", "正在处理种子");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentActionJobID) + "/download/control", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: action})});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", (body && (body.error || body.code)) || "没有执行。");
        return;
      }
      var done = {pause: "种子已暂停，文件保留。", resume: "种子已继续。", recheck: "已请求重新校验。", delete: "已从 qBittorrent 移除，文件保留。"};
      setStatus("ok", done[action] || "已完成。");
    } catch (err) {
      setStatus("fail", "请求中断。");
    } finally {
      setBusy(false);
    }
    openJob(currentJob.job_id);
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
      html += "<p class=\"meta\">" + (body.sites || []).map(function (site) { return escapeText(site.site) + "：" + escapeText(site.status) + "（" + site.candidate_count + " 个结果" + (site.exact ? "，有精确匹配" : "") + (site.limited ? "，未搜全" : "") + (site.reason ? "，" + escapeText(site.reason) : "") + ")"; }).join("<br>") + "</p>";
      var candidates = body.candidates || [];
      if (candidates.length) {
        html += "<p>候选（只列出已观测到的结果）：</p>";
        candidates.forEach(function (candidate, index) {
          html += '<label class="candidate"><input type="radio" name="review-candidate" value="' + escapeText(candidate.candidate_id) + '" data-site="' + escapeText(candidate.site) + '"' + (index === 0 ? " checked" : "") + "> " + (candidate.exact ? '<span class="candidate-tag">精确</span> ' : '') + escapeText(candidate.title) + " · " + escapeText((candidate.artists || []).join(" / ")) + " · " + escapeText(candidate.site) + (candidate.quality ? " · " + escapeText(candidate.quality) : "") + (candidate.free ? " · FL 免费" : "") + (candidate.seeders != null ? " · 做种 " + candidate.seeders : "") + "</label>";
        });
      } else {
        html += "<p>没有观测到的候选。</p>";
      }
      var allowed = {};
      (body.actions || []).forEach(function (action) { allowed[action.action] = action; });
      html += '<p id="review-actions">';
      if (body.ai_review_available) html += '<button type="button" id="review-ai">AI 核对单项</button> ';
      if (allowed.accept && allowed.accept.allowed) html += '<button type="button" id="review-accept" class="primary">接受选中</button> ';
      else if (allowed.accept) html += '<span class="meta">接受不可用：' + escapeText(allowed.accept.reason || "") + "</span> ";
      if (allowed.retry && allowed.retry.allowed) html += '<button type="button" id="review-retry">重试</button> ';
      else if (allowed.retry) html += '<span class="meta">重试不可用：' + escapeText(allowed.retry.reason || "") + "</span> ";
      if (allowed.skip && allowed.skip.allowed) html += '<button type="button" id="review-skip">跳过</button>';
      else if (allowed.skip) html += '<span class="meta">跳过不可用：' + escapeText(allowed.skip.reason || "") + "</span>";
      // A saved torrent is the point where the album can go to qBittorrent.
      // The token checkpoint has the bytes too: downloading spends nothing
      // more, so it is offered there as well.
      if (body.review_kind === "candidate" || body.review_kind === "token") html += ' <button type="button" id="review-download" class="primary">继续下载</button>';
      html += "</p>";
      box.innerHTML = html;
      var aiReview = document.getElementById("review-ai");
      if (aiReview) aiReview.addEventListener("click", function () { reviewAction("ai"); });
      var accept = document.getElementById("review-accept");
      if (accept) accept.addEventListener("click", function () { reviewAction("accept"); });
      var retry = document.getElementById("review-retry");
      if (retry) retry.addEventListener("click", function () { reviewAction("retry"); });
      var skip = document.getElementById("review-skip");
      if (skip) skip.addEventListener("click", function () { reviewAction("skip"); });
      var download = document.getElementById("review-download");
      if (download) download.addEventListener("click", function () { reviewDownload(); });
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
    var autoSkipped = false;
    if (action === "accept") {
      var picked = document.querySelector("input[name=\"review-candidate\"]:checked");
      if (!picked) { setStatus("fail", "先选一个候选。"); return; }
      site = picked.getAttribute("data-site");
      candidateID = picked.value;
    }
    setBusy(true);
    setStatus("info", action === "ai" ? "AI 正在核对候选" : "正在处理核对");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentReviewJobID) + "/review", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: action, site: site, candidate_id: candidateID})});
      var body = await response.json();
      if (!response.ok && action === "ai" && body && body.code === "AI_NO_CONFIRMED_MATCH") {
        setStatus("info", "AI 没有确认候选，正在自动结束本轮核对");
        response = await fetch("/v1/jobs/" + encodeURIComponent(currentReviewJobID) + "/review", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: "skip"})});
        body = await response.json();
        if (response.ok && body && body.ok && body.stage === "skipped") {
          autoSkipped = true;
          setStatus("review", "AI 没有确认安全候选，已自动结束本轮，避免继续留在待核对。");
        }
      }
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", (body && body.message) || ("核对操作失败。" + (body && body.code ? "（" + body.code + "）" : "")));
        return;
      }
      if (Number(body.retry_after_ns || 0) > Date.now()) {
        setStatus("ok", "已排除人工核对并进入后台冷却重试，将在 " + new Date(Number(body.retry_after_ns) / 1000000).toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"}) + " 自动继续。");
      } else if (body.held || body.stage === "needs_review") {
        setStatus("review", body.message || "已提交余额对账，稍后自动收敛。");
      } else if (body.stage === "fallback") {
        setStatus("ok", "已自动切换流媒体回退，未重复使用令牌。");
      } else if (body.stage === "skipped") {
        setStatus("review", autoSkipped ? "AI 没有确认安全候选，已自动结束本轮，避免继续留在待核对。" : "没有可安全执行的后续路径，已自动结束。");
      } else {
        setStatus(action === "skip" ? "review" : "ok", action === "accept" ? "已接受候选，继续处理。" : (action === "retry" ? "已自动重试。" : "已自动处理。"));
      }
    } catch (err) {
      setStatus("fail", "核对操作中断。");
    } finally {
      setBusy(false);
    }
    openJob(currentJob.job_id);
  }
  async function reviewDownload() {
    if (!currentJob || loading) return;
    setBusy(true);
    setStatus("info", "正在交给 qBittorrent");
    try {
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentReviewJobID) + "/download", {method: "POST", headers: {"Content-Type": "application/json"}, body: "{}"});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", (body && body.error) || ("没有开始下载。" + (body && body.code ? "（" + body.code + "）" : "")));
        return;
      }
      setStatus("ok", "已交给 qBittorrent。");
    } catch (err) {
      setStatus("fail", "下载请求中断。");
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
      // A downloading job is controlled on both sides: the workflow task and
      // the qBittorrent torrent. Cancel means "remove the torrent, keep the
      // files" there; pausing the task alone would leave the torrent running.
      var downloading = currentActionStage === "downloading";
      var path = "/v1/jobs/" + encodeURIComponent(currentActionJobID) + (downloading ? "/download/control" : "/control");
      var payload = downloading ? {action: action === "cancel" ? "delete" : action} : {action: action};
      var response = await fetch(path, {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(payload)});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) {
        setStatus("fail", (body && (body.error || body.code)) || "操作失败。");
        return;
      }
      var done = {pause: "已暂停。", resume: "已继续。", cancel: "已取消。", delete: "已取消，并从 qBittorrent 移除该种子（已下载的文件保留）。"};
      setStatus("ok", done[payload.action] || "已完成。");
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
      var response = await fetch("/v1/jobs/" + encodeURIComponent(currentActionJobID) + "/review", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({action: "retry"})});
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
  async function reloadThen(kind, text) {
    setBusy(false);
    await load();
    setStatus(kind, text);
  }
  async function load(quiet) {
    if (loading) return;
    loading = true;
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
      if (view === "search" || (view === "tasks" && !quiet)) {
        var playlistResponse = await fetch("/v1/playlists", {headers: {Accept: "application/json"}});
        var playlistBody = await playlistResponse.json();
        playlistRows = playlistBody && Array.isArray(playlistBody.playlists) ? playlistBody.playlists : [];
      }
      if (view === "tasks" && !quiet) {
        var subscriptionResponse = await fetch("/v1/subscriptions", {headers: {Accept: "application/json"}});
        var subscriptionBody = await subscriptionResponse.json();
        subscriptionRows = subscriptionBody && Array.isArray(subscriptionBody.subscriptions) ? subscriptionBody.subscriptions : [];
      }
      if (view === "settings") {
        var proxyResponse = await fetch("/v1/settings/proxy", {headers: {Accept: "application/json"}});
        var proxyBody = await proxyResponse.json();
        outboundProxy = proxyBody && proxyBody.ok ? proxyBody.proxy : null;
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
        solverRuntime = solverBody && solverBody.ok ? solverBody.runtime : null;
        var neteaseResponse = await fetch("/v1/settings/streaming/netease", {headers: {Accept: "application/json"}});
        var neteaseBody = await neteaseResponse.json();
        neteaseStatus = neteaseBody && neteaseBody.ok ? neteaseBody : null;
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
        var automationResponse = await fetch("/v1/library/automation", {headers: {Accept: "application/json"}});
        var automationBody = await automationResponse.json();
        libraryAutomation = automationBody && automationBody.ok ? automationBody : null;
        var libraryResponse = await fetch("/v1/library", {headers: {Accept: "application/json"}});
        var libraryBody = await libraryResponse.json();
        libraryRows = libraryBody && Array.isArray(libraryBody.entries) ? libraryBody.entries : [];
        var transcodeResponse = await fetch("/v1/library/transcode", {headers: {Accept: "application/json"}});
        var transcodeBody = await transcodeResponse.json();
        transcodeRows = transcodeBody && Array.isArray(transcodeBody.receipts) ? transcodeBody.receipts : [];
        try {
          var duplicateStatusPath = "/v1/library/duplicates/status" + (duplicateRows.length ? "" : "?include=result");
          var duplicateStatusResponse = await fetch(duplicateStatusPath, {headers: {Accept: "application/json"}});
          var duplicateStatusBody = await duplicateStatusResponse.json();
          if (duplicateStatusBody && duplicateStatusBody.ok) {
            duplicateScanStatus = duplicateStatusBody.status || duplicateScanStatus;
            if (duplicateStatusBody.result && Array.isArray(duplicateStatusBody.result.groups)) duplicateRows = duplicateStatusBody.result.groups;
          }
        } catch (duplicateStatusErr) { /* The task list remains usable if status is temporarily unavailable. */ }
      } else if (view === "logs") {
        var logResponse = await fetch("/v1/logs", {headers: {Accept: "application/json"}});
        var logBody = await logResponse.json();
        logRows = logBody && Array.isArray(logBody.tasks) ? logBody.tasks : [];
        logEvents = logBody && Array.isArray(logBody.events) ? logBody.events : [];
      } else {
        var jobsURL = view === "review" ? "/v1/jobs?review=1&counts=1" : (view === "tasks" ? "/v1/jobs?roots=1&counts=1&live=1" : "/v1/jobs?live=1&counts=1");
        var response = await fetch(jobsURL, {headers: {Accept: "application/json"}});
        var body = await response.json();
        jobs = body && Array.isArray(body.jobs) ? body.jobs : [];
        jobsNext = body && body.next ? body.next : "";
        globalJobCounts = body.counts || null;
        activeCount = body.active_count || 0;
        pipelinePaused = !!body.pipeline_paused;
        autoReviewStatus = body.auto_review || autoReviewStatus;
        lastLoadedAt = Date.now();
      }
      if (detail.hidden) render();
      arm();
    } catch (err) {
      if (!quiet) setStatus("fail", "任务列表没有读到。");
    } finally {
      loading = false;
      if (!quiet) setBusy(false);
    }
  }
  async function loadMoreReview() {
    if (!jobsNext || loading) return;
    setBusy(true);
    setStatus("info", "正在读取更多待核对");
    try {
      var response = await fetch("/v1/jobs?" + (view === "tasks" ? "roots=1&live=1" : "review=1") + "&after=" + encodeURIComponent(jobsNext), {headers: {Accept: "application/json"}});
      var body = await response.json();
      if (!response.ok || !body || !body.ok) { setStatus("fail", "没有读到更多任务。"); return; }
      var seen = {};
      jobs.forEach(function (job) { seen[job.job_id || job.root_id || ""] = true; });
      (body.jobs || []).forEach(function (job) {
        var key = job.job_id || job.root_id || "";
        if (!key || !seen[key]) { jobs.push(job); seen[key] = true; }
      });
      jobsNext = body.next || "";
      render();
      setStatus("ok", "已加载 " + (body.jobs || []).length + " 条。");
    } catch (err) {
      setStatus("fail", "读取中断。");
    } finally {
      setBusy(false);
    }
  }
  var timer = 0;
  function arm() {
    clearTimeout(timer);
    if (view !== "tasks" && view !== "queue" && view !== "review") return;
    var queued = checkCount(globalJobCounts || {}, "queued");
    if (activeCount <= 0 && queued <= 0) return;
    timer = setTimeout(function () { load(true); }, view === "tasks" ? (activeCount > 0 ? 5000 : 30000) : (activeCount > 0 ? 2500 : 15000));
  }
  document.getElementById("nav").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (!button || !button.getAttribute("data-view") || loading) return;
    view = button.getAttribute("data-view");
    detail.hidden = true;
    load();
  });
  list.addEventListener("click", function (event) {
    var filterButton = event.target.closest("button[data-task-filter]");
    if (filterButton) {
      taskFilter = filterButton.getAttribute("data-task-filter") || "all";
      selectedJobs = {};
      render();
      return;
    }
    if (event.target.closest("#review-more") && !loading) { loadMoreReview(); return; }
    var selectButton = event.target.closest("button[data-batch-select]");
    if (selectButton) { setBatchSelection(selectButton.getAttribute("data-batch-select")); return; }
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
    updateBatchCount();
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
  document.getElementById("torrent-pause").addEventListener("click", function () { torrentControl("pause"); });
  document.getElementById("torrent-resume").addEventListener("click", function () { torrentControl("resume"); });
  document.getElementById("torrent-recheck").addEventListener("click", function () { torrentControl("recheck"); });
  document.getElementById("torrent-delete").addEventListener("click", function () { torrentControl("delete"); });
  load();
})();
`
