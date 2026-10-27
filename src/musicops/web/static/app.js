"use strict";
const $ = (id) => document.getElementById(id),
  node = (tag, text, cls) => {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    return e;
  };
const apiURL = (path) => {
  const url = new URL(path, location.href);
  url.username = "";
  url.password = "";
  return url.href;
};
if (new URL(location.href).username)
  history.replaceState(null, "", apiURL(location.href));
let csrf = "",
  config = null,
  refs = new Set(),
  targets = [],
  offset = 0,
  total = 0,
  selected = new Set(),
  jobs = [],
  polling = false,
  reviewQueue = [],
  currentReview = null,
  loginTimer = null,
  jobsGeneration = 0,
  taskStats = null;
const pages = {
  playlist: "歌单入库",
  library: "音乐库整理",
  search: "专辑与艺人检索",
  tasks: "任务中心",
  settings: "连接与设置",
};
const platforms = {
  netease: "网易云音乐",
  qqmusic: "QQ 音乐",
  kuwo: "酷我",
  kugou: "酷狗",
  migu: "咪咕",
  qishui: "汽水",
  bilibili: "bilibili",
  applemusic: "Apple Music",
  spotify: "Spotify",
  youtube: "YouTube Music",
  tidal: "TIDAL",
  qobuz: "Qobuz",
  deezer: "Deezer",
  soundcloud: "SoundCloud",
  joox: "JOOX",
  audius: "Audius",
  qianqian: "千千音乐",
  streetvoice: "街声",
};
const engineSources = {
  netease: "netease",
  qqmusic: "qq",
  qishui: "soda",
  applemusic: "apple",
};
const playlistPlatform = $("playlist-platform");
playlistPlatform.append(new Option("自动识别平台", "auto"));
for (const key of [
  "netease",
  "qqmusic",
  "kuwo",
  "kugou",
  "migu",
  "qishui",
  "bilibili",
  "applemusic",
  "spotify",
  "youtube",
])
  playlistPlatform.append(new Option(platforms[key], key));
function notice(message) {
  $("notice").textContent = message || "";
}
function action(text, fn, cls) {
  const e = node("button", text, cls);
  e.type = "button";
  e.onclick = () => guard(e, fn);
  return e;
}
async function guard(button, fn) {
  button.disabled = true;
  try {
    await fn();
  } catch (e) {
    notice(e.message);
  } finally {
    button.disabled = false;
  }
}
async function api(command, payload = {}) {
  const request_id =
    crypto.randomUUID?.() ||
    Array.from(crypto.getRandomValues(new Uint8Array(16)), (n) =>
      n.toString(16).padStart(2, "0"),
    ).join("");
  const body = JSON.stringify({ command, payload, request_id });
  const send = () =>
    fetch(apiURL("api/command"), {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-MusicOps-CSRF": csrf },
      body,
    });
  let response = await send(),
    data = await response.json();
  if (response.status === 403 && data.error_class === "CSRF_EXPIRED") {
    csrf = (await (await fetch(apiURL("api/bootstrap"))).json()).csrf;
    response = await send();
    data = await response.json();
  }
  if (!response.ok || !data.ok) throw new Error(data.error || "后台请求未完成");
  return data.result;
}
const delay = (ms) => new Promise((r) => setTimeout(r, ms));
async function operation(command, payload = {}, done) {
  const r = await api(command, payload);
  if (!r.accepted) {
    if (done) await done(r);
    return r;
  }
  notice("任务已提交，后台继续处理，可在任务中心查看。");
  for (let i = 0; i < 1800; i++) {
    await delay(1500);
    const { task } = await api("get_task", { task_id: r.task_id });
    if (
      [
        "completed",
        "failed",
        "blocked",
        "cancelled",
        "recovery_required",
      ].includes(task.state)
    ) {
      if (task.state !== "completed")
        throw new Error(task.error || "任务需要核对");
      if (done) await done(task.result || {});
      return task.result || {};
    }
  }
  throw new Error("任务仍在后台运行，请在任务中心查看。");
}
function showPage(key) {
  for (const id in pages) $(id).hidden = id !== key;
  $("title").textContent = pages[key];
  for (const e of $("nav").children)
    e.classList.toggle("active", e.dataset.page === key);
  if (key === "tasks") refreshJobs();
}
for (const [key, title] of Object.entries(pages)) {
  const e = action(title, () => showPage(key));
  e.dataset.page = key;
  $("nav").append(e);
}
function loaded(result) {
  targets = result.targets || [];
  $("tracks").replaceChildren();
  for (const t of targets) {
    const tr = node("tr"),
      td = node("td"),
      check = node("input");
    check.type = "checkbox";
    check.dataset.id = t.target_id;
    td.append(check);
    tr.append(
      td,
      ...[t.raw_title, (t.artists || []).join(" / "), t.album || "待识别"].map(
        (v) => node("td", v),
      ),
    );
    $("tracks").append(tr);
  }
  $("playlist-count").textContent =
    `共 ${targets.length} 首${result.complete === false ? " · 来源仅返回部分曲目" : ""}`;
  notice(result.reason || "已读取，请勾选要入库的曲目。");
}
async function upload(file, kind) {
  const limit = kind === "playlist" ? 20 * 1024 * 1024 : 10000000;
  if (file.size > limit)
    throw new Error(
      kind === "playlist" ? "歌单文件不能超过 20MB" : "封面不能超过 10MB",
    );
  const content = await new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result.split(",")[1]);
    reader.onerror = reject;
    reader.readAsDataURL(file);
  });
  const r = await fetch(apiURL("api/upload"), {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-MusicOps-CSRF": csrf },
    body: JSON.stringify({ filename: file.name, kind, content }),
  });
  const data = await r.json();
  if (!data.ok) throw new Error(data.error);
  return data.file_path;
}
$("import-file").onclick = () => $("playlist-file").click();
function textOrder() {
  return new Promise((resolve) => {
    const dialog = $("text-order-dialog");
    dialog.returnValue = "";
    dialog.onclose = () =>
      resolve(
        ["title-artist", "artist-title"].includes(dialog.returnValue)
          ? dialog.returnValue
          : null,
      );
    dialog.showModal();
  });
}
$("playlist-file").onchange = async () => {
  const file = $("playlist-file").files[0];
  $("playlist-file").value = "";
  if (!file) return;
  await guard($("import-file"), async () => {
    const isText = file.name.toLowerCase().endsWith(".txt");
    const order = isText ? await textOrder() : null;
    if (isText && !order) return;
    const path = await upload(file, "playlist");
    const args = { file_path: path, platform: $("playlist-platform").value };
    if (isText) args.text_order = order;
    await operation("refresh_playlist", args, loaded);
  });
};
$("parse").onclick = () =>
  guard($("parse"), () =>
    operation(
      "refresh_playlist",
      {
        playlist_input: $("playlist-url").value.trim(),
        platform: $("playlist-platform").value,
      },
      loaded,
    ),
  );
$("cache").onclick = () =>
  guard($("cache"), async () => loaded(await api("get_playlist_targets")));
function when(value) {
  if (!value) return "—";
  const date = new Date(typeof value === "number" ? value * 1000 : value);
  return isNaN(date)
    ? "—"
    : date.toLocaleString([], {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
      });
}
async function loadSubscriptions() {
  const { subscriptions } = await api("list_subscriptions");
  $("subscriptions").replaceChildren(
    ...subscriptions.map((s) => {
      const tr = node("tr"),
        controls = node("td"),
        id = { subscription_id: s.subscription_id },
        title = node(
          "td",
          (s.name || s.playlist_input) + (s.enabled ? "" : "（已暂停）"),
        );
      title.title = s.playlist_input;
      controls.append(
        action("立即检查", async () => {
          notice("正在检查歌单…");
          await operation("check_subscription_now", id, (r) =>
            notice(r.reason || "检查完成"),
          );
          await loadSubscriptions();
        }),
        action(s.enabled ? "暂停" : "恢复", async () => {
          await api("save_subscription", { ...id, enabled: !s.enabled });
          await loadSubscriptions();
        }),
        action("删除", async () => {
          if (!confirm("删除后不再检查这个歌单；已入库的音乐不受影响。")) return;
          await api("delete_subscription", id);
          await loadSubscriptions();
        }),
      );
      tr.append(
        title,
        node("td", s.interval_hours + " 小时"),
        node("td", when(s.last_checked_at)),
        node("td", s.last_reason || "等待首次检查"),
        node("td", s.enabled ? when(s.next_check_at) : "—"),
        controls,
      );
      return tr;
    }),
  );
}
$("subscribe").onclick = () =>
  guard($("subscribe"), async () => {
    const text = $("playlist-url").value.trim();
    if (!text) throw new Error("请先在上方粘贴要订阅的歌单链接");
    await api("save_subscription", {
      playlist_input: text,
      platform: $("playlist-platform").value,
      import_existing: $("subscription-existing").checked,
      interval_hours: Number($("subscription-hours").value),
      prefer_full_album: $("playlist-full-album").checked,
    });
    notice("已订阅，马上进行首次检查。");
    await loadSubscriptions();
  });
setInterval(() => {
  if (!$("playlist").hidden) loadSubscriptions().catch(() => {});
}, 30000);
$("select-tracks").onclick = () => {
  const checks = [...$("tracks").querySelectorAll("input")],
    all = checks.every((e) => e.checked);
  checks.forEach((e) => (e.checked = !all));
};
async function start(scene, extra = {}) {
  if (!config?.workflow.setup_done) {
    showPage("settings");
    notice("请先选择音乐库并保存设置。");
    return;
  }
  const ids = [...$("tracks").querySelectorAll("input:checked")].map(
    (e) => e.dataset.id,
  );
  if (scene === "playlist" && !ids.length) throw new Error("请先勾选曲目");
  showPage("tasks");
  await operation(
    "start_workflow",
    {
      scene,
      target_ids: ids,
      prefer_24bit: true,
      prefer_full_album:
        scene === "playlist" && $("playlist-full-album").checked,
      ...extra,
    },
    (r) => notice(r.message),
  );
  await refreshJobs();
}
$("import-tracks").onclick = () =>
  guard($("import-tracks"), () => start("playlist"));
$("complete").onclick = () =>
  guard($("complete"), () =>
    start("complete", { refresh_resources: $("refresh-resources").checked }),
  );
$("upgrade").onclick = () => guard($("upgrade"), () => start("upgrade"));
$("index").onclick = () =>
  guard($("index"), () =>
    operation(
      "index_library",
      { root: config.paths.library, lightweight: true },
      () => notice("本地专辑资料库已更新"),
    ),
  );
$("albums").onclick = () =>
  guard($("albums"), async () => {
    const r = await api("list_library_albums");
    const area = $("album-results");
    area.hidden = false;
    area.replaceChildren(
      node("h3", `本地专辑 · ${r.total} 张（本页最多 200 张）`),
    );
    for (const a of r.albums) {
      const row = node("div", undefined, "row");
      row.append(
        node("span", a.album || a.title || a.album_id),
        action("补全整张专辑", () => start("fill", { album_id: a.album_id })),
        action("单曲升级完整专辑", () =>
          start("expand", { album_id: a.album_id, retire_single: false }),
        ),
      );
      area.append(row);
    }
  });
$("fill-albums").onclick = () => $("albums").click();
$("expand-albums").onclick = () => $("albums").click();
$("restore-duplicates").onclick = () =>
  guard($("restore-duplicates"), async () => {
    const r = await api("duplicate_history");
    if (!r.transactions.length) throw new Error("暂无可恢复的去重合并");
    if (!confirm("恢复上次合并的原文件？如果目标发生冲突会停止并保留双方。"))
      return;
    for (const tx of [...r.transactions].reverse())
      await operation("restore_duplicates", { tx_id: tx.tx_id });
    notice("上次合并已恢复");
  });
$("duplicates").onclick = () =>
  guard($("duplicates"), () =>
    operation("scan_duplicates", {}, (r) => {
      const area = $("duplicate-results");
      area.replaceChildren(
        node("p", r.message || `发现 ${r.groups?.length || 0} 组候选`),
      );
      for (const group of r.groups || []) {
        const row = node("details");
        row.append(
          node("summary", group.label || group.group_id),
          node("pre", JSON.stringify(group.files || group, null, 2)),
        );
        if (group.ready || group.state === "ready")
          row.append(
            action("合并此组（保留恢复副本）", async () => {
              if (confirm("确认合并已校验的重复内容？"))
                await operation(
                  "merge_duplicates",
                  { scan_id: r.scan_id, group_ids: [group.group_id] },
                  () => notice("合并完成"),
                );
            }),
          );
        area.append(row);
      }
    }),
  );
let artistContinue = true;
function sourceRow(c) {
  const row = node("div", undefined, "row");
  row.append(
    node(
      "span",
      (c.source || "") +
        " · " +
        c.title +
        " · " +
        (c.media?.format || "") +
        " " +
        (c.media?.encoding || "") +
        " · 做种 " +
        (c.seeders || 0),
    ),
  );
  const raw = c.detail_url || c.url;
  try {
    const u = new URL(raw);
    if (
      u.protocol === "https:" &&
      [
        "www.open.cd",
        "open.cd",
        "dicmusic.com",
        "orpheus.network",
        "redacted.sh",
      ].includes(u.hostname)
    ) {
      const link = node("a", "打开资源详情");
      link.href = u.href;
      link.target = "_blank";
      link.rel = "noopener noreferrer";
      row.append(link);
    }
  } catch (_) {}
  return row;
}
async function search(candidate_id, options = {}) {
  const args = {
    mode: $("search-mode").value,
    query: $("search-query").value.trim(),
    artist: $("search-artist").value.trim(),
    ...options,
  };
  if (candidate_id) args.candidate_id = candidate_id;
  const r = await operation("manual_album_search", args);
  const area = $("search-results");
  area.replaceChildren(node("p", r.message || r.status));
  const names = {
      "open.cd": "OpenCD",
      dicmusic: "DicMusic",
      orpheus: "Orpheus",
      redacted: "RED",
    },
    stateNames = {
      OK: "检索完成",
      NO_RESOURCE: "没有资源",
      DISABLED: "未启用",
      AUTH_FAILED: "登录失效",
      AUTH_REQUIRED: "需要登录",
      NETWORK_ERROR: "连接失败",
      RATE_LIMITED: "限流",
    };
  if (r.source_outcomes)
    area.append(
      node(
        "p",
        Object.entries(r.source_outcomes)
          .map(
            ([key, o]) =>
              (names[key] || key) + "：" + (stateNames[o.status] || o.status),
          )
          .join("；"),
      ),
    );
  if (r.mode === "artist") {
    const controls = node("div", undefined, "row");
    controls.append(
      action("停止自动继续读取", () => {
        artistContinue = false;
      }),
      action("继续读取下一页", () =>
        search(null, {
          mode: "artist",
          query: r.query,
          artist_result_id: r.artist_result_id,
        }),
      ),
      action("重试未完成站点", () =>
        search(null, {
          mode: "artist",
          query: r.query,
          artist_result_id: r.artist_result_id,
          retry_sites: true,
        }),
      ),
      action("AI 辅助识别分组", () =>
        search(null, {
          mode: "artist",
          query: r.query,
          artist_result_id: r.artist_result_id,
          force_ai: true,
        }),
      ),
    );
    area.append(controls);
    for (const album of r.albums || []) {
      const details = node("details");
      details.append(
        node("summary", album.title + (album.uncertain ? " · 归属待核对" : "")),
      );
      for (const version of album.versions || []) {
        const d = node("details");
        d.append(
          node(
            "summary",
            version.label +
              " · " +
              (version.year || "年份未知") +
              " · " +
              (version.catalogue || "目录号未知"),
          ),
        );
        for (const c of version.candidates || []) {
          const row = sourceRow(c);
          row.append(
            action("修正专辑分组", async () => {
              const name = prompt("请输入已核对的专辑名称", album.title);
              if (name)
                await search(null, {
                  mode: "artist",
                  query: r.query,
                  artist_result_id: r.artist_result_id,
                  group_override: { candidate_id: c.candidate_id, album: name },
                });
            }),
          );
          d.append(row);
        }
        details.append(d);
      }
      area.append(details);
    }
    if (r.has_more && artistContinue) {
      await delay(300);
      return search(null, {
        mode: "artist",
        query: r.query,
        artist_result_id: r.artist_result_id,
      });
    }
    return;
  }
  for (const a of r.albums || []) {
    const row = node("div", undefined, "row");
    row.append(
      node(
        "span",
        (a.artists || []).join(" / ") + " · " + (a.album || a.title),
      ),
      action("检索此发行", () => search(a.candidate_id)),
    );
    area.append(row);
  }
  for (const c of r.candidates || []) area.append(sourceRow(c));
  if (!r.albums?.length && !r.candidates?.length)
    area.append(
      node("p", "没有可用结果，可调整关键词或检查已启用站点的连接状态。"),
    );
}
$("search-button").onclick = () =>
  guard($("search-button"), () => {
    artistContinue = true;
    return search();
  });
const states = {
  completed: "已完成",
  failed: "失败",
  needs_review: "需要核对",
  searching: "搜索中",
  identifying: "识别中",
  downloading: "下载中",
  enrich: "补全资料",
  fallback: "流媒体回退",
  queued: "排队",
  running: "进行中",
  cancelled: "已取消",
};
async function refreshJobs() {
  if (!config) return;
  const generation = ++jobsGeneration;
  const result = await api("list_workflows", {
    offset,
    state: $("task-state").value,
    view: $("task-view").value,
    query: $("task-query").value.trim(),
  });
  if (generation !== jobsGeneration) return;
  jobs = result.jobs;
  total = result.total;
  taskStats = result;
  const area = $("job-list"),
    scroll = window.scrollY,
    expanded = new Set(
      [...area.querySelectorAll(".job")]
        .filter((e) => e.querySelector("details[open]"))
        .map((e) => e.dataset.id),
    );
  const previousCards = new Map(
    [...area.children].map((e) => [e.dataset.id, e]),
  );
  let position = 0;
  for (const j of jobs) {
    const signature = JSON.stringify(j),
      previous = previousCards.get(j.job_id);
    previousCards.delete(j.job_id);
    if (previous?.musicopsSignature === signature) {
      previous.querySelector("input[type=checkbox]").checked = selected.has(
        j.job_id,
      );
      if (area.children[position] !== previous)
        area.insertBefore(previous, area.children[position] || null);
      position++;
      continue;
    }
    const card = node("div", undefined, "job"),
      head = node("div", undefined, "job-head"),
      check = node("input");
    card.dataset.id = j.job_id;
    check.type = "checkbox";
    check.checked = selected.has(j.job_id);
    check.onchange = () => {
      check.checked ? selected.add(j.job_id) : selected.delete(j.job_id);
      updateTaskSummary();
    };
    head.append(
      check,
      node("strong", j.label || j.job_id, "job-title"),
      node("span", j.paused ? "已暂停" : states[j.stage] || j.stage),
      action("核对 / 详情", () => openReview(j.job_id)),
    );
    card.append(head, node("small", j.message || ""));
    const bar = node("progress");
    bar.max = 100;
    bar.value = j.progress || 0;
    card.append(bar);
    if (j.chain?.length) {
      const chain = node("details");
      chain.open = expanded.has(j.job_id);
      chain.append(node("summary", "识别、搜索与处理步骤"));
      for (const step of j.chain)
        chain.append(
          action(
            (step.task_type || "步骤") +
              " · " +
              (states[step.stage] || step.stage),
            () => openReview(step.job_id),
          ),
        );
      card.append(chain);
    }
    card.musicopsSignature = signature;
    if (previous) previous.replaceWith(card);
    if (area.children[position] !== card)
      area.insertBefore(card, area.children[position] || null);
    position++;
  }
  for (const old of previousCards.values()) old.remove();
  window.scrollTo(0, scroll);
  $("page-info").textContent = `${total} 项 · 第 ${offset / 200 + 1} 页`;
  $("previous").disabled = offset === 0;
  $("next").disabled = offset + 200 >= total;
  updateTaskSummary();
}
function updateTaskSummary() {
  if (taskStats)
    $("task-summary").textContent =
      `进行中 ${taskStats.active} · 暂停 ${taskStats.paused_count} · 已完成 ${taskStats.finished} · 已勾选 ${selected.size}`;
}
async function idsFor(state) {
  return (
    await api("list_workflows", { selection: true, state, view: "albums" })
  ).job_ids;
}
async function bulk(actionName, ids) {
  for (let i = 0; i < ids.length; i += 500)
    await api("bulk_workflows", {
      action: actionName,
      job_ids: ids.slice(i, i + 500),
    });
  await refreshJobs();
}
$("task-actions").onclick = async (e) => {
  const b = e.target.closest("button");
  if (!b) return;
  await guard(b, async () => {
    const a = b.dataset.action,
      ids = [...selected];
    if (a === "clear") {
      selected.clear();
      return refreshJobs();
    }
    if (a === "select") {
      const all = jobs.every((j) => selected.has(j.job_id));
      jobs.forEach((j) =>
        all ? selected.delete(j.job_id) : selected.add(j.job_id),
      );
      return refreshJobs();
    }
    if (a === "all-ai") {
      notice((await api("queue_ai_reviews", { all: true })).message);
      return;
    }
    if (a === "all-retry") return bulk("retry", await idsFor("failed"));
    if (a === "all-review") {
      reviewQueue = await idsFor("review");
      return nextReview();
    }
    if (!ids.length) throw new Error("请先勾选任务");
    if (a === "ai") {
      notice((await api("queue_ai_reviews", { job_ids: ids })).message);
      return;
    }
    if (a === "review") {
      reviewQueue = ids;
      return nextReview();
    }
    if (
      a === "delete" &&
      !confirm("删除所选任务记录？已入库音乐与做种文件会保留。")
    )
      return;
    await bulk(a, ids);
  });
};
for (const key of ["task-state", "task-view", "task-query"])
  $(key).onchange = () => {
    offset = 0;
    refreshJobs().catch((e) => notice(e.message));
  };
$("previous").onclick = () => {
  offset = Math.max(0, offset - 200);
  refreshJobs().catch((e) => notice(e.message));
};
$("next").onclick = () => {
  offset += 200;
  refreshJobs().catch((e) => notice(e.message));
};
for (const verb of ["pause", "resume"])
  $(verb + "-pipeline").onclick = () =>
    guard($(verb + "-pipeline"), async () => {
      await api(verb + "_pipeline");
      notice(verb === "pause" ? "后台将在安全阶段暂停" : "后台已继续");
    });
async function nextReview() {
  if (!reviewQueue.length) {
    $("review-dialog").close();
    notice("本次手动核对已完成");
    return;
  }
  await openReview(reviewQueue.shift());
}
async function submitReview(payload) {
  const result = await api("review_workflow", {
    job_id: currentReview.job_id,
    expected_revision: currentReview.review_revision,
    ...payload,
  });
  $("review-result").textContent = result.message || "已提交";
  if (reviewQueue.length) await nextReview();
  else {
    $("review-dialog").close();
    await refreshJobs();
  }
}
async function openReview(id) {
  const { job } = await api("get_workflow", { job_id: id });
  currentReview = job;
  $("review-title").textContent = job.label || id;
  $("review-note").textContent = (job.message || "") + " " + (job.detail || "");
  $("review-result").textContent = "";
  const area = $("review-content");
  area.replaceChildren();
  const metadata = job.prepared || [];
  if (job.review_kind === "album_scope") {
    const options = node("div", undefined, "scope-options");
    area.append(
      node("p", "歌单原曲：" + metadata.map((t) => t.raw_title).join(" / ")),
      options,
    );
    options.append(
      action("保留原单曲发行，继续检索", () =>
        submitReview({ choice: "keep_single" }),
      ),
    );
    for (const c of job.catalog_choices || []) {
      options.append(
        action(
          "下载完整专辑：" +
            (c.album || c.title) +
            " · " +
            (c.artists || []).join(" / ") +
            " · " +
            (c.date || c.year || "年份待确认") +
            " · " +
            (c.tracklist || []).length +
            " 首" +
            ((c.release_secondary_types || []).length
              ? " · " + c.release_secondary_types.join(" / ")
              : ""),
          () => submitReview({ choice: "playlist_album:" + c.candidate_id }),
        ),
      );
    }
    if (!$("review-dialog").open) $("review-dialog").showModal();
    return;
  }
  if (metadata.length) {
    const table = node("table"),
      head = node("tr");
    ["歌曲", "艺人（ / 分隔）", "专辑", "时长（秒）", "轨号", "碟号"].forEach(
      (s) => head.append(node("th", s)),
    );
    table.append(head);
    for (const t of metadata) {
      const tr = node("tr");
      for (const [key, value] of Object.entries({
        title: t.raw_title,
        artist: (t.artists || []).join(" / "),
        album: t.album,
        duration: t.duration,
        track: t.track,
        disc: t.disc,
      })) {
        const td = node("td"),
          input = node("input");
        input.value = value || "";
        input.dataset.key = key;
        td.append(input);
        tr.append(td);
      }
      table.append(tr);
    }
    area.append(
      table,
      action("保存修正并继续下一张", () =>
        submitReview({
          choice: "edit_metadata",
          metadata: [...table.querySelectorAll("tr")].slice(1).map((tr) => {
            const r = {};
            tr.querySelectorAll("input").forEach(
              (e) => (r[e.dataset.key] = e.value),
            );
            return r;
          }),
        }),
      ),
    );
  }
  for (const c of job.catalog_choices || []) {
    if (["fill", "expand"].includes(job.scene))
      area.append(
        action(
          "采用完整发行：" +
            (c.title || c.album) +
            " · " +
            (c.artists || []).join(" / "),
          () =>
            submitReview({ choice: "completion_release:" + c.candidate_id }),
        ),
      );
    else
      area.append(
        action("填入专辑和艺人供核对：" + (c.album || c.title), () => {
          area
            .querySelectorAll("input[data-key=album]")
            .forEach((e) => (e.value = c.album || c.title));
          area
            .querySelectorAll("input[data-key=artist]")
            .forEach((e) => (e.value = (c.artists || []).join(" / ")));
        }),
      );
  }
  if (["resources", "cover", "locks", "edition"].includes(job.review_kind)) {
    area.append(
      action(
        job.review_kind === "edition"
          ? "确认此发行版本并继续替换"
          : job.review_kind === "locks"
            ? "解锁所需资料字段并继续"
            : "重新检查并继续",
        () =>
          submitReview({
            choice: job.review_kind === "locks" ? "unlock" : "retry",
          }),
      ),
    );
    if (job.review_kind === "resources" || job.review_kind === "cover")
      area.append(
        action("本次跳过缺失资料", () => submitReview({ choice: "skip" })),
      );
    if (job.review_kind === "cover") {
      const file = node("input");
      file.type = "file";
      file.accept = "image/jpeg,image/png,image/webp";
      file.onchange = async () => {
        if (file.files[0])
          await guard(file, async () =>
            submitReview({ cover_path: await upload(file.files[0], "cover") }),
          );
      };
      area.append(node("p", "上传你已核对的专辑封面（至少 300px）："), file);
    }
  }
  const chosen = new Set();
  for (const c of job.choices || []) {
    const row = node("label"),
      check = node("input");
    check.type = "checkbox";
    check.onchange = () =>
      check.checked
        ? chosen.add(c.candidate_id)
        : chosen.delete(c.candidate_id);
    row.append(
      check,
      node(
        "span",
        (c.source || "") +
          " · " +
          c.title +
          " · " +
          JSON.stringify(c.media || {}),
      ),
    );
    area.append(row);
  }
  if (job.choices?.length)
    area.append(
      action("采用勾选音源", () =>
        submitReview({
          choice: job.version_choice
            ? [...chosen]
            : chosen.size === 1
              ? [...chosen][0]
              : [...chosen],
        }),
      ),
    );
  const row = node("div", undefined, "row"),
    query = node("input");
  query.value = job.search_query || job.label || "";
  row.append(
    query,
    action("按修正关键词重新搜索", () =>
      submitReview({ choice: "research", query: query.value }),
    ),
    action("AI 辅助核对", async () =>
      notice(
        (await api("queue_ai_reviews", { job_ids: [id], retry: true })).message,
      ),
    ),
    action("重试此任务", () => submitReview({ choice: "retry" })),
  );
  if (job.review_kind === "search")
    row.append(
      action("确认回退流媒体", () =>
        submitReview({ choice: "streaming_fallback" }),
      ),
    );
  if (job.review_kind === "token")
    row.append(
      action("仅本次不使用令牌", () =>
        submitReview({ choice: "nonfree_once" }),
      ),
      action("本批次不使用令牌", () =>
        submitReview({ choice: "nonfree_batch" }),
      ),
    );
  area.append(row);
  if (!$("review-dialog").open) $("review-dialog").showModal();
}
$("review-close").onclick = () => {
  $("review-dialog").close();
  reviewQueue = [];
};
function field(parent, id, title, value = "", type = "text", help = "") {
  const label = node("label", undefined, "field"),
    input = node("input");
  input.id = "f-" + id;
  input.type = type;
  if (type === "checkbox") input.checked = !!value;
  else input.value = value ?? "";
  label.append(node("span", title), input);
  parent.append(label);
  if (help) parent.append(node("small", help));
  return input;
}
const val = (id) => $("f-" + id).value,
  checked = (id) => $("f-" + id).checked;
function secret(parent, id, title, ref, help = "") {
  const e = field(parent, id, title, "", "password", help);
  e.placeholder = refs.has(ref)
    ? "•••••••• · 已保存，留空保留"
    : "未填写，可跳过";
  return e;
}
function settings() {
  const form = $("setup");
  form.onsubmit = (e) => {
    e.preventDefault();
    saveSettings().catch((error) => notice(error.message));
  };
  form.replaceChildren();
  const card = (title, help) => {
    const e = node("fieldset", undefined, "card");
    e.append(node("h3", title));
    if (help) e.append(node("p", help));
    form.append(e);
    return e;
  };
  let p = card(
    "音乐库与媒体工具",
    "下载临时文件放在音乐库同级的 MusicOps 目录；升级保留现有设置。",
  );
  field(p, "library", "音乐库路径", config.paths.library);
  for (const key of ["ffmpeg", "ffprobe"])
    field(p, key, key + " 程序", config.paths.tools[key]);
  field(
    p,
    "roon",
    "Roon 监听同一目录",
    config.music_servers.roon.enabled,
    "checkbox",
  );
  p = card(
    "四个资源站",
    "按需启用，Cookie-Editor 导出 JSON；留空保留已保存登录态。",
  );
  for (const key of ["open.cd", "dicmusic", "orpheus", "redacted"]) {
    field(
      p,
      key + "-enabled",
      key + " 参与检索",
      config.providers[key].enabled,
      "checkbox",
    );
    secret(
      p,
      key + "-cookie",
      key + " Cookie",
      config.providers[key].credential_ref,
    );
  }
  field(
    p,
    "fl-unlimited",
    "FL 全部（不限制批次次数）",
    config.workflow.fl_unlimited,
    "checkbox",
  );
  const count = field(
    p,
    "fl-count",
    "每批次最多 FL 次数",
    config.workflow.fl_per_batch,
    "number",
  );
  const unlimited = $("f-fl-unlimited");
  unlimited.onchange = () => (count.disabled = unlimited.checked);
  unlimited.onchange();
  p = card(
    "qBittorrent",
    "启用 WebUI，完成文件校验后自动开始下载；仅使用 MusicOps 标签。",
  );
  field(p, "qbt-url", "WebUI 地址", config.qbt.base_url);
  secret(p, "qbt-user", "用户名", config.qbt.credential_ref);
  secret(p, "qbt-pass", "密码", config.qbt.credential_ref);
  p.append(
    action("测试已保存的连接", async () =>
      notice((await api("test_qbt_connection")).message || "连接测试完成"),
    ),
  );
  const edition = document.createElement("select");
  edition.id = "f-edition-policy";
  for (const [key, title] of Object.entries({
    quality: "音质最好优先",
    complete: "歌曲最全优先",
    special: "特殊版优先",
  }))
    edition.append(new Option(title, key));
  edition.value = config.workflow.edition_policy;
  p.append(node("p", "同一发行的音源筛选策略"), edition);
  field(
    p,
    "artist-folders",
    "资料补全后把根目录下的专辑移到“音乐人/专辑”文件夹（合作专辑用 A／B）",
    config.workflow.artist_folders !== false,
    "checkbox",
  );
  p.append(
    action("将已有下载标签统一为 MusicOps", () =>
      operation("normalize_qbt_tags", {}, (r) =>
        notice(r.message || "标签已整理"),
      ),
    ),
  );
  p = card(
    "流媒体与歌单授权",
    "网易云与其他平台统一在这里管理。无账号可跳过。网页登录 Cookie 与 Spotify / Apple / YouTube API 授权属于不同类型，按平台要求填写。",
  );
  field(
    p,
    "netease-enabled",
    "启用网易云兜底",
    config.workflow.netease_fallback,
    "checkbox",
  );
  field(
    p,
    "netease-api",
    "网易云 API（可选）",
    config.workflow.netease_api_url,
  );
  field(
    p,
    "netease-import",
    "客户端下载目录（可选）",
    config.workflow.netease_import_dir,
  );
  secret(
    p,
    "netease-cookie",
    "网易云 Cookie JSON",
    config.netease.credential_ref,
  );
  field(
    p,
    "stream-enabled",
    "启用多平台兜底",
    config.streaming.enabled,
    "checkbox",
  );
  field(
    p,
    "stream-managed",
    "使用内置 go-music-dl",
    config.streaming.managed,
    "checkbox",
  );
  field(p, "stream-url", "自建引擎完整地址", config.streaming.base_url);
  const order = node("div");
  order.id = "stream-order";
  p.append(
    node("p", "按显示顺序依次尝试，勾选参与兜底的平台；通过上下按钮调整顺序。"),
    order,
  );
  for (const key of [
    ...new Set([...config.streaming.order, ...Object.keys(platforms)]),
  ]) {
    const row = node("div", undefined, "row"),
      check = node("input");
    check.type = "checkbox";
    check.checked = config.streaming.order.includes(key);
    check.dataset.source = key;
    row.append(
      check,
      node("span", platforms[key] || key),
      action("↑", () => {
        if (row.previousElementSibling)
          order.insertBefore(row, row.previousElementSibling);
      }),
      action("↓", () => {
        if (row.nextElementSibling)
          order.insertBefore(row.nextElementSibling, row);
      }),
    );
    order.append(row);
  }
  p.append(action("平台 Cookie / 扫码登录", login));
  field(
    p,
    "musicdl-enabled",
    "启用自行安装的 musicdl",
    config.streaming.musicdl.enabled,
    "checkbox",
  );
  $("f-musicdl-enabled").onchange = () => {
    if (checked("musicdl-enabled")) $("f-stream-enabled").checked = true;
  };
  field(
    p,
    "musicdl-python",
    "musicdl 所在 Python 路径",
    config.streaming.musicdl.python_path,
    "text",
    "musicdl 不随程序捆绑；请在自己的独立环境安装。新增平台可在上方勾选。",
  );
  for (const s of [
    "tidal",
    "qobuz",
    "deezer",
    "soundcloud",
    "joox",
    "audius",
    "qianqian",
    "streetvoice",
  ])
    secret(
      p,
      "musicdl-" + s,
      platforms[s] + " Cookie JSON 对象",
      config.streaming.musicdl.credential_refs[s],
    );
  for (const key of Object.keys(config.playlist_sources)) {
    secret(
      p,
      "playlist-" + key,
      (platforms[key] || key) + " 歌单授权",
      config.playlist_sources[key].credential_ref,
    );
  }
  p = card("FlareSolverr", "遇到浏览器挑战时使用；不重放下载或 FL 令牌请求。");
  field(
    p,
    "solver-enabled",
    "启用浏览器验证",
    config.browser_solver.enabled,
    "checkbox",
  );
  field(
    p,
    "solver-managed",
    "使用内置服务",
    config.browser_solver.managed,
    "checkbox",
  );
  field(p, "solver-url", "外部服务地址", config.browser_solver.base_url);
  p.append(
    action("测试已保存服务", async () =>
      notice((await api("test_browser_solver")).message || "浏览器测试完成"),
    ),
  );
  p = card(
    "歌词、封面与 AI",
    "测试使用当前填写内容，无需先保存；测试不修改音乐。",
  );
  field(
    p,
    "lyric-url",
    "歌词接口地址",
    config.lyrics.primary.base_url.includes(".example.invalid")
      ? ""
      : config.lyrics.primary.base_url,
  );
  secret(
    p,
    "lyric-key",
    "完整 Authorization 值",
    config.lyrics.primary.auth_secret_ref,
  );
  p.append(
    action("测试歌词接口", async () =>
      notice(
        (
          await api("test_connection", {
            kind: "lyrics",
            settings: {
              base_url: val("lyric-url"),
              authorization: val("lyric-key"),
            },
          })
        ).message,
      ),
    ),
  );
  field(p, "ai-enabled", "启用 AI 辅助", config.ai.enabled, "checkbox");
  field(
    p,
    "ai-url",
    "AI API 地址",
    config.ai.base_url.includes(".example.invalid") ? "" : config.ai.base_url,
  );
  field(p, "ai-model", "模型名称", config.ai.model);
  secret(p, "ai-key", "API Key", config.ai.auth_secret_ref);
  p.append(
    action("测试 AI 与当前模型", async () =>
      notice(
        (
          await api("test_connection", {
            kind: "ai",
            settings: {
              base_url: val("ai-url"),
              model: val("ai-model"),
              key: val("ai-key"),
            },
          })
        ).message,
      ),
    ),
  );
  field(
    p,
    "catalog-web",
    "允许公开资料网站兜底",
    config.catalog.web_enabled,
    "checkbox",
  );
  field(
    p,
    "simplified",
    "优先写入简体中文资料",
    config.metadata.prefer_simplified,
    "checkbox",
  );
  secret(
    p,
    "discogs-token",
    "Discogs Token（可选）",
    config.catalog.discogs.credential_ref,
  );
  p = card(
    "Plex 与 Navidrome",
    "服务需要读取同一音乐库。Plex 测试会列出可用音乐库 ID，不触发扫描。",
  );
  const plex = config.music_servers.plex;
  field(p, "plex-enabled", "启用 Plex", plex.enabled, "checkbox");
  field(p, "plex-url", "Plex 地址", plex.url);
  field(p, "plex-section", "音乐库 ID", plex.section_id);
  secret(p, "plex-token", "X-Plex-Token", plex.auth_secret_ref);
  p.append(
    action("测试并读取 Plex 音乐库", async () => {
      const r = await api("test_connection", {
        kind: "plex",
        settings: { url: val("plex-url"), token: val("plex-token") },
      });
      notice(
        r.message +
          "\n" +
          r.sections.map((s) => s.id + " · " + s.name).join("\n"),
      );
    }),
  );
  const nav = config.music_servers.navidrome;
  field(p, "nav-enabled", "启用 Navidrome", nav.enabled, "checkbox");
  field(p, "nav-url", "Navidrome 地址", nav.url);
  field(p, "nav-user", "用户名", nav.username);
  secret(p, "nav-pass", "密码", nav.password_ref);
  form.append(action("保存全部设置", saveSettings, "primary"));
}
async function saveSettings() {
  const sites = {};
  for (const s of ["open.cd", "dicmusic", "orpheus", "redacted"])
    sites[s] = {
      enabled: checked(s + "-enabled"),
      cookie_json: val(s + "-cookie"),
    };
  const playlist_credentials = {};
  for (const s of Object.keys(config.playlist_sources))
    playlist_credentials[s] = val("playlist-" + s);
  const r = await api("save_setup", {
    library: val("library"),
    ffmpeg: val("ffmpeg"),
    ffprobe: val("ffprobe"),
    sites,
    netease_cookie_json: val("netease-cookie"),
    playlist_credentials,
    qbt: {
      url: val("qbt-url"),
      username: val("qbt-user"),
      password: val("qbt-pass"),
    },
    catalog_web: checked("catalog-web"),
    prefer_simplified: checked("simplified"),
    discogs_token: val("discogs-token"),
    workflow: {
      edition_policy: val("edition-policy"),
      netease_fallback: checked("netease-enabled"),
      netease_api_url: val("netease-api"),
      netease_import_dir: val("netease-import"),
      fl_unlimited: checked("fl-unlimited"),
      fl_per_batch: Number(val("fl-count")),
      artist_folders: checked("artist-folders"),
    },
    streaming: {
      musicdl: {
        enabled: checked("musicdl-enabled"),
        python_path: val("musicdl-python"),
        cookies: Object.fromEntries(
          [
            "tidal",
            "qobuz",
            "deezer",
            "soundcloud",
            "joox",
            "audius",
            "qianqian",
            "streetvoice",
          ].map((s) => [s, val("musicdl-" + s)]),
        ),
      },
      enabled: checked("stream-enabled"),
      managed: checked("stream-managed"),
      base_url: val("stream-url"),
      order: [...$("stream-order").querySelectorAll("input:checked")].map(
        (e) => e.dataset.source,
      ),
    },
    browser_solver: {
      enabled: checked("solver-enabled"),
      managed: checked("solver-managed"),
      base_url: val("solver-url"),
    },
    lyrics: { url: val("lyric-url"), authorization: val("lyric-key") },
    ai: {
      enabled: checked("ai-enabled"),
      url: val("ai-url"),
      model: val("ai-model"),
      key: val("ai-key"),
    },
    servers: {
      roon: checked("roon"),
      plex: {
        enabled: checked("plex-enabled"),
        url: val("plex-url"),
        section_id: val("plex-section"),
        token: val("plex-token"),
      },
      navidrome: {
        enabled: checked("nav-enabled"),
        url: val("nav-url"),
        username: val("nav-user"),
        password: val("nav-pass"),
      },
    },
  });
  config = r.config.values;
  notice(r.message);
  await refresh();
  settings();
}
let loginGeneration = 0;
function login() {
  clearInterval(loginTimer);
  $("login-platform").replaceChildren();
  for (const [key, title] of Object.entries(platforms).filter(([k]) =>
    [
      "netease",
      "qqmusic",
      "kuwo",
      "kugou",
      "migu",
      "qishui",
      "bilibili",
      "applemusic",
    ].includes(k),
  )) {
    const e = node("option", title);
    e.value = engineSources[key] || key;
    $("login-platform").append(e);
  }
  $("login-platform").append(new Option("QQ 音乐（微信扫码）", "qq_wx"));
  loginChanged();
  $("login-note").textContent =
    "公开音源可跳过；不支持二维码的平台请使用 Cookie。";
  $("login-dialog").showModal();
}
function loginChanged() {
  loginGeneration++;
  clearTimeout(loginTimer);
  $("login-image").hidden = true;
  $("login-cookie").value = "";
  const source = $("login-platform").value;
  $("login-save").disabled = source === "qq_wx";
  $("login-cookie").placeholder = refs.has(
    "vault://engine/" + (source === "qq_wx" ? "qq" : source),
  )
    ? "•••••••• · 已保存，留空保留"
    : "未填写，可跳过";
}
$("login-platform").onchange = loginChanged;
$("login-close").onclick = () => {
  $("login-dialog").close();
};
$("login-dialog").onclose = () => {
  loginGeneration++;
  clearTimeout(loginTimer);
};
$("login-save").onclick = () =>
  guard($("login-save"), async () => {
    const r = await api("engine_control", {
      action: "cookie",
      source: $("login-platform").value,
      cookie: $("login-cookie").value,
    });
    $("login-cookie").value = "";
    $("login-cookie").placeholder = "•••••••• · 已保存";
    $("login-note").textContent = r.message;
  });
$("login-qr").onclick = () =>
  guard($("login-qr"), async () => {
    clearTimeout(loginTimer);
    const generation = ++loginGeneration,
      source = $("login-platform").value;
    const result = await api("engine_control", { action: "create", source });
    if (generation !== loginGeneration) return;
    if (!result.image) {
      $("login-note").textContent =
        "此平台未提供可用二维码，请使用 Cookie 登录";
      return;
    }
    $("login-image").src = result.image;
    $("login-image").hidden = false;
    $("login-note").textContent = "请用对应平台 App 扫码并确认登录。";
    async function poll() {
      try {
        const r = await api("engine_control", {
          action: "poll",
          source,
          session_id: result.session_id,
        });
        if (generation !== loginGeneration) return;
        $("login-note").textContent = r.message;
        if (r.saved) {
          $("login-image").hidden = true;
          refs.add("vault://engine/" + (source === "qq_wx" ? "qq" : source));
        } else loginTimer = setTimeout(poll, 3000);
      } catch (e) {
        if (generation === loginGeneration)
          $("login-note").textContent = e.message;
      }
    }
    loginTimer = setTimeout(poll, 3000);
  });
let connectionMessage = null;
async function refresh() {
  if (polling) return;
  polling = true;
  try {
    const r = await api("get_status", { compact: true });
    if (connectionMessage === $("notice").textContent) notice("");
    connectionMessage = null;
    const initial = !config;
    config = r.config.values;
    refs = new Set(
      (r.secret_refs || []).map((v) => (typeof v === "string" ? v : v.ref)),
    );
    $("connection").textContent = "后台已连接";
    $("connection").title = config.paths.library;
    $("catalog-summary").textContent =
      `${r.local_catalog.album_count} 张专辑 · ${r.library_file_count} 首音源`;
    if (initial) settings();
    if (!$("tasks").hidden) await refreshJobs();
  } catch (e) {
    try {
      const status = await (await fetch(apiURL("api/startup"))).json();
      connectionMessage = status.message || e.message;
    } catch (_) {
      connectionMessage = e.message;
    }
    $("connection").textContent = "后台连接未完成";
    notice(connectionMessage);
  } finally {
    polling = false;
  }
}
$("refresh").onclick = () => refresh();
(async () => {
  try {
    const boot = await (await fetch(apiURL("api/bootstrap"))).json();
    csrf = boot.csrf;
    $("app-version").textContent =
      "MusicOps " + (boot.version || "") + " · 你的音乐收藏";
    showPage("playlist");
    await refresh();
    loadSubscriptions().catch(() => {});
    setInterval(() => {
      if (!document.hidden) refresh();
    }, 5000);
  } catch (e) {
    notice(e.message);
  }
})();
