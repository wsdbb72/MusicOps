package app

// appCSS is the local MusicOps design system. The visual direction is a
// compact music operations desk: quiet neutral chrome, one cobalt operational
// accent, flat hairline surfaces and a single live status band. Linear-style
// density keeps the existing vertical layout while making active work,
// selection and primary actions scannable without decorative cards. The
// reference pass follows unslop by Damir Sokolovsky (@heydamir), CC BY 4.0.
const appCSS = `/* ---------- tokens ---------- */
:root{
  color-scheme:light;
  --font-ui:"Segoe UI Variable Text","Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei","Noto Sans SC",system-ui,sans-serif;
  --font-mono:"Cascadia Mono","SFMono-Regular",Consolas,"Liberation Mono",monospace;
  --t-xs:.75rem;--t-sm:.8125rem;--t-md:.875rem;--t-lg:1rem;--t-xl:1.25rem;--t-2xl:1.625rem;
  --lh-tight:1.25;--lh-base:1.5;
  --s-1:.25rem;--s-2:.5rem;--s-3:.75rem;--s-4:1rem;--s-5:1.5rem;--s-6:2rem;--s-7:3rem;
  --r-xs:4px;--r-sm:8px;--r-md:10px;--r-lg:14px;--r-pill:999px;
  --bg:#f4f5f7;--surface:#fff;--surface-2:#f7f8fa;--sunken:#edf0f4;
  --fg:#17191d;--fg-muted:#5d6470;--fg-subtle:#747c88;
  --line:#e2e4e8;--line-strong:#cdd2d9;
  --accent:#2457b8;--accent-hover:#1d4695;--accent-fg:#fff;--accent-soft:#edf2ff;
  --warn:#875a12;--warn-soft:#f7eedc;
  --info:#2457b8;--info-soft:#edf2ff;
  --danger:#a32a2a;--danger-soft:#faeaea;
  --ring:0 0 0 2px var(--surface),0 0 0 4px rgba(36,87,184,.38);
  --shadow-1:0 1px 2px rgba(18,22,28,.04);
  --shadow-2:0 12px 32px rgba(18,22,28,.12);
  --dur:150ms;--ease:cubic-bezier(.22,1,.36,1);
}
@media (prefers-color-scheme:dark){
  :root{
    color-scheme:dark;
    --bg:#0e1117;--surface:#151a21;--surface-2:#1a2029;--sunken:#222a35;
    --fg:#eef0f3;--fg-muted:#aeb4bd;--fg-subtle:#858d98;
    --line:#2a2e35;--line-strong:#3d434c;
    --accent:#7fa6ff;--accent-hover:#9ab8ff;--accent-fg:#0b1734;--accent-soft:#1a2947;
    --warn:#e1ad62;--warn-soft:#332716;
    --info:#9ab8ff;--info-soft:#172540;
    --danger:#ed8c88;--danger-soft:#3a2020;
    --ring:0 0 0 2px var(--surface),0 0 0 4px rgba(127,166,255,.46);
    --shadow-1:0 1px 2px rgba(0,0,0,.34);
    --shadow-2:0 14px 38px rgba(0,0,0,.52);
  }
}

/* ---------- base ---------- */
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;font-family:var(--font-ui);font-size:var(--t-md);line-height:var(--lh-base);color:var(--fg);background:var(--bg);-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
h1,h2,h3{margin:0 0 var(--s-2);font-weight:550;line-height:var(--lh-tight);letter-spacing:0}
h1{font-size:var(--t-2xl)}
h2{font-size:var(--t-lg)}
h3{font-size:var(--t-md);color:var(--fg-muted)}
p{margin:0 0 var(--s-3)}
p:last-child{margin-bottom:0}
a{color:var(--accent);text-underline-offset:2px}
code,kbd,samp{font-family:var(--font-mono);font-size:.92em;background:var(--sunken);border-radius:var(--r-xs);padding:.08em .34em}
hr{border:0;border-top:1px solid var(--line);margin:var(--s-4) 0}
::selection{background:var(--accent-soft);color:var(--fg)}
:where(button,a,input,select,textarea,summary,[tabindex]):focus-visible{outline:none;box-shadow:var(--ring);border-radius:var(--r-sm)}
[hidden]{display:none!important}

/* ---------- shell ---------- */
.skip{position:absolute;left:var(--s-4);top:var(--s-4);z-index:60;transform:translateY(-240%);background:var(--surface);color:var(--fg);border:1px solid var(--line-strong);border-radius:var(--r-sm);padding:var(--s-2) var(--s-3);text-decoration:none;box-shadow:var(--shadow-2);transition:transform var(--dur) var(--ease)}
.skip:focus{transform:none}
.app{display:grid;grid-template-rows:auto minmax(0,1fr);min-width:0;min-height:100dvh}
.app-bar{padding:0;border-bottom:1px solid var(--line);background:var(--surface);position:sticky;top:0;z-index:30}
.app-bar-inner{display:grid;grid-template-columns:auto minmax(0,1fr);min-width:0;align-items:end;gap:var(--s-4) var(--s-5);max-width:78rem;margin:0 auto;padding:var(--s-2) var(--s-5) 0}
.mark{margin:0;display:flex;align-items:center;gap:var(--s-2);color:var(--fg);font-weight:600;font-size:var(--t-md);white-space:nowrap;min-width:0;overflow:hidden;padding-bottom:var(--s-3)}
.mark span{flex:0 0 auto;white-space:nowrap}
.mark .brand-name{color:var(--fg);font-weight:600;letter-spacing:0}
.mark .brand-sub{color:var(--fg-subtle);font-weight:400;font-size:var(--t-xs);letter-spacing:0}
.brand-icon{flex:0 0 auto;width:24px;height:24px;border-radius:6px}
.mark .app-version{color:var(--fg-subtle);background:transparent;border-radius:0;padding:0 0 0 var(--s-2);border-left:1px solid var(--line);font-family:var(--font-mono);font-weight:400;font-size:var(--t-xs);letter-spacing:0}
@media (max-width:640px){.mark .brand-sub{display:none}}

.pick-list{display:grid;gap:2px;max-height:22rem;overflow:auto;border:1px solid var(--line);border-radius:var(--r-md);padding:var(--s-2) var(--s-3);margin:var(--s-2) 0}
.pick-row{display:flex;align-items:center;gap:var(--s-2);font-size:var(--t-sm);padding:2px 0}
.pick-row input{flex:0 0 auto}

/* ---------- navigation ---------- */
nav{display:flex;align-items:stretch;gap:var(--s-1);overflow-x:auto;scrollbar-width:none;min-width:0}
nav::-webkit-scrollbar{display:none}
nav button{position:relative;flex:0 0 auto;border:0;border-radius:0;background:transparent;color:var(--fg-muted);padding:10px 10px 12px;font-size:var(--t-sm);font-weight:500;min-height:40px}
nav button:hover{background:var(--surface-2);color:var(--fg)}
nav button[aria-current="page"]{background:transparent;color:var(--fg);font-weight:600}
nav button[aria-current="page"]::after{content:"";position:absolute;left:10px;right:10px;bottom:-1px;height:2px;border-radius:2px 2px 0 0;background:var(--accent)}
nav button:disabled{opacity:.5}

/* ---------- main ---------- */
main{display:block;width:100%;min-width:0;max-width:78rem;margin:0 auto;padding:var(--s-5) var(--s-5) var(--s-7)}
main:focus{outline:none}
.view-head{display:flex;align-items:flex-end;justify-content:space-between;gap:var(--s-4);margin:0 0 var(--s-4);padding:var(--s-2) 0 0}
.view-head h1{margin:0;font-size:var(--t-2xl);font-weight:610}
.view-subtitle{margin:var(--s-1) 0 0;color:var(--fg-muted);font-size:var(--t-sm)}
.view-live{display:inline-flex;align-items:center;gap:var(--s-2);min-height:32px;padding:0 var(--s-3);border:1px solid var(--line);border-radius:var(--r-pill);background:var(--surface);color:var(--fg-muted);font-size:var(--t-xs);font-weight:550;white-space:nowrap}
.view-live.running{color:var(--accent);border-color:color-mix(in srgb,var(--accent) 28%,var(--line));background:var(--accent-soft)}
.view-live.paused{color:var(--warn);background:var(--warn-soft)}
.live-dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:currentColor;box-shadow:0 0 0 3px color-mix(in srgb,currentColor 14%,transparent)}
#status{display:flex;align-items:center;gap:var(--s-2);min-height:28px;margin:0 0 var(--s-4);color:var(--fg-muted);font-size:var(--t-sm)}
#status:empty{display:none;min-height:0;margin:0}
#status::before{content:"";flex:0 0 auto;width:6px;height:6px;border-radius:50%;background:currentColor}
#status.ok{color:var(--accent)}
#status.fail{color:var(--danger)}
#status.review{color:var(--warn)}
#status.info{color:var(--info)}

/* ---------- buttons ---------- */
button{font:inherit;font-size:var(--t-sm);font-weight:500;display:inline-flex;align-items:center;justify-content:center;gap:var(--s-2);border:1px solid var(--line-strong);background:var(--surface);color:var(--fg);border-radius:var(--r-sm);min-height:32px;padding:5px 11px;cursor:pointer;transition:background var(--dur) var(--ease),border-color var(--dur) var(--ease),color var(--dur) var(--ease),transform var(--dur) var(--ease)}
button:hover{background:var(--surface-2);border-color:var(--fg-subtle)}
button:active{transform:translateY(1px)}
button:disabled{opacity:.48;cursor:not-allowed;transform:none}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}
button.primary:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button.danger{color:var(--danger);border-color:color-mix(in srgb,var(--danger) 42%,var(--line));background:var(--danger-soft)}
button.danger:hover{background:var(--danger-soft);border-color:var(--danger)}
button.quiet{border-color:transparent;background:transparent;color:var(--fg-muted)}
button.quiet:hover{background:var(--surface-2);border-color:var(--line);color:var(--fg)}
button[aria-current="page"]{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}

/* ---------- forms ---------- */
form{margin:0}
label{display:block;margin:0 0 var(--s-3);color:var(--fg-muted);font-size:var(--t-sm)}
label>input,label>select,label>textarea{margin-top:var(--s-1)}
input[type="text"],input[type="password"],input[type="url"],input[type="number"],input[type="search"],select,textarea{width:100%;min-height:36px;padding:6px 10px;border:1px solid var(--line-strong);border-radius:var(--r-sm);font:inherit;font-size:var(--t-md);color:var(--fg);background:var(--surface);transition:border-color var(--dur) var(--ease)}
input[type="text"]:hover,input[type="password"]:hover,input[type="url"]:hover,input[type="number"]:hover,select:hover,textarea:hover{border-color:var(--fg-subtle)}
input[type="text"]:focus,input[type="password"]:focus,input[type="url"]:focus,input[type="number"]:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);box-shadow:var(--ring)}
textarea{min-height:96px;resize:vertical}
label:has(> input[type="checkbox"]),label:has(> input[type="radio"]){display:flex;align-items:flex-start;gap:var(--s-2);color:var(--fg);font-size:var(--t-md);line-height:1.45}
input[type="checkbox"],input[type="radio"]{flex:0 0 auto;width:16px;height:16px;margin:2px 0 0;accent-color:var(--accent)}
fieldset{border:1px solid var(--line);border-radius:var(--r-md);padding:var(--s-3) var(--s-4) var(--s-2);margin:0 0 var(--s-3);background:var(--surface-2)}
legend{padding:0 var(--s-2);color:var(--fg-muted);font-size:var(--t-sm);font-weight:500}
fieldset label:last-child{margin-bottom:var(--s-2)}
#search-sites label{display:inline-flex;margin-right:var(--s-3)}
#search-sites br{display:none}

/* ---------- cards and sections ---------- */
.cards{display:grid;gap:var(--s-3);min-width:0}
@media (min-width:52rem){.cards{grid-template-columns:minmax(0,1fr);align-items:start}}
article{display:flex;flex-direction:column;min-width:0;background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);padding:var(--s-4);box-shadow:none;transition:border-color var(--dur) var(--ease),background var(--dur) var(--ease)}
article:has(.card-link):hover{border-color:var(--line-strong)}
article>h2:first-child{margin-top:0}
.section-heading{display:flex;align-items:flex-start;justify-content:space-between;gap:var(--s-3);margin-bottom:var(--s-3)}
.section-heading h2{margin:0}
.section-copy{margin:2px 0 0;color:var(--fg-muted);font-size:var(--t-sm)}
.run-state{display:inline-flex;align-items:center;gap:6px;color:var(--fg-subtle);font-size:var(--t-xs);white-space:nowrap}
.run-state::before{content:"";width:6px;height:6px;border-radius:50%;background:var(--fg-subtle)}
.run-state.running{color:var(--accent)}
.run-state.running::before{background:var(--accent)}
.maintenance-status{padding:10px var(--s-3);margin:0 0 var(--s-3);border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface-2);color:var(--fg-muted);font-size:var(--t-sm);overflow-wrap:anywhere}
.maintenance-status.running{border-color:color-mix(in srgb,var(--accent) 32%,var(--line));background:var(--accent-soft);color:var(--fg)}
.option-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:var(--s-2) var(--s-4);margin:0 0 var(--s-3)}
.option-grid label{margin:0}
.action-row{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-2);margin:var(--s-3) 0}
.action-row.compact{margin:0}
.action-row:last-child{margin-bottom:0}
.card-link{display:block;width:100%;border:0;background:transparent;padding:0;text-align:left;color:inherit;font:inherit;border-radius:var(--r-sm);justify-content:flex-start}
.card-link:hover{background:transparent;border-color:transparent}
.card-link:active{transform:none}
.card-link h2{margin:0 0 var(--s-1);color:var(--fg);font-size:var(--t-lg);font-weight:600}
.card-link p{margin:0;color:var(--fg-muted);font-size:var(--t-sm)}
.card-link p:first-of-type{margin-bottom:var(--s-1)}

/* ---------- selection, meta and batches ---------- */
.selection-toolbar{display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);padding:var(--s-2) 0;border-top:1px solid var(--line);border-bottom:1px solid var(--line)}
.selection-count{color:var(--fg-muted);font-size:var(--t-xs);font-variant-numeric:tabular-nums;white-space:nowrap}
.selection-count.all{color:var(--accent);font-weight:600}
.pagination{display:flex;align-items:center;gap:var(--s-2);padding:var(--s-2) 0}
.file-picker{display:grid;gap:1px;max-height:25rem;overflow:auto;border:1px solid var(--line);border-radius:var(--r-sm);background:var(--line)}
.file-row{display:grid!important;grid-template-columns:16px minmax(0,1fr);align-items:center!important;gap:10px!important;margin:0!important;padding:8px 10px!important;background:var(--surface);color:var(--fg-muted)!important;font-family:var(--font-mono);font-size:var(--t-xs)!important;line-height:1.4!important}
.file-row:hover{background:var(--surface-2);color:var(--fg)}
.file-row input{grid-row:1;width:16px;height:16px;margin:0}
.file-row span{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.meta{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-1) var(--s-2);margin:var(--s-2) 0;color:var(--fg-muted);font-size:var(--t-sm)}
.meta:empty{display:none}
.pill{display:inline-flex;align-items:center;gap:4px;border-radius:var(--r-xs);padding:1px 7px;background:var(--sunken);color:var(--fg-muted);font-size:var(--t-xs);font-weight:500;white-space:nowrap}
.pill.review,.pill.partial{background:var(--warn-soft);color:var(--warn)}
.pill.queue{background:var(--info-soft);color:var(--info)}
.pill.fail{background:var(--danger-soft);color:var(--danger)}
.batch{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-2);margin:0 0 var(--s-3);padding:var(--s-2) var(--s-3);border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface-2)}
.batch>span{color:var(--fg-muted);font-size:var(--t-sm);margin-right:var(--s-1)}
.batch button[data-batch-select]{background:var(--surface);color:var(--fg-muted)}
.batch button[data-batch-action=resume],.batch button[data-batch-action=retry]{background:var(--accent-soft);border-color:color-mix(in srgb,var(--accent) 34%,var(--line));color:var(--accent)}

/* ---------- detail ---------- */
#detail{background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);padding:var(--s-4) var(--s-5) var(--s-5)}
#back{margin-bottom:var(--s-3)}
#detail-title{font-size:var(--t-xl);margin:0 0 var(--s-2)}
dl{display:grid;grid-template-columns:minmax(72px,120px) minmax(0,1fr);gap:var(--s-2) var(--s-4);margin:var(--s-4) 0;align-items:baseline}
dt{color:var(--fg-subtle);font-size:var(--t-sm)}
dd{margin:0;color:var(--fg);overflow-wrap:anywhere}
#detail-actions,#torrent-actions{display:flex;flex-wrap:wrap;gap:var(--s-2);margin:var(--s-3) 0}
#torrent-actions{padding-top:var(--s-3);border-top:1px solid var(--line)}
.review-box{margin-top:var(--s-4);border:1px solid var(--line);border-left:2px solid var(--warn);border-radius:var(--r-sm);padding:var(--s-4);background:var(--surface-2)}
.review-box h3{margin-top:0;color:var(--warn)}
.review-box .candidate{display:flex;align-items:flex-start;gap:var(--s-2);margin:0 0 var(--s-2);padding:var(--s-2) var(--s-3);border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface);cursor:pointer;transition:border-color var(--dur) var(--ease),background var(--dur) var(--ease)}
.review-box .candidate:hover{border-color:var(--line-strong)}
.review-box .candidate:has(input:checked){border-color:var(--accent);background:var(--accent-soft)}
.candidate-tag{display:inline-block;padding:0 5px;border-radius:var(--r-xs);background:var(--accent-soft);color:var(--accent);font-size:var(--t-xs);font-weight:600;white-space:nowrap}
#review-actions{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-2);margin:var(--s-3) 0 0}

/* ---------- notes and library ---------- */
#note{border:1px solid var(--line);border-radius:var(--r-sm);padding:var(--s-4);color:var(--fg-muted);background:var(--surface-2)}
.library-track{font-family:var(--font-mono);font-size:var(--t-xs);color:var(--fg-muted)}
#list{min-width:0}

/* ---------- task operations ---------- */
.ops-overview{--ops-text:#f4f7ff;--ops-muted:#aab7cc;background:#18212d;color:var(--ops-text);border:1px solid #26364a;border-radius:var(--r-lg);padding:var(--s-5);margin:0 0 var(--s-4);overflow:hidden}
.ops-main{display:grid;grid-template-columns:auto minmax(0,1fr) auto;align-items:center;gap:var(--s-4);margin-bottom:var(--s-5)}
.ops-state{display:inline-flex;align-items:center;gap:var(--s-2);color:var(--ops-muted);font-size:var(--t-xs);font-weight:600;white-space:nowrap}
.ops-overview.running .ops-state{color:#9cbaff}.ops-overview.paused .ops-state{color:#e7bb78}
.ops-copy h2{margin:0;color:var(--ops-text);font-size:var(--t-xl);font-weight:600}.ops-copy p{margin:var(--s-1) 0 0;color:var(--ops-muted);font-size:var(--t-sm)}
.ops-actions{display:flex;align-items:center;gap:var(--s-2)}.ops-actions button{background:transparent;border-color:#3b4b60;color:var(--ops-text)}.ops-actions button:hover{background:#233044;border-color:#52667f}.ops-actions button.primary{background:#7fa6ff;border-color:#7fa6ff;color:#0b1734}.ops-actions button.primary:hover{background:#9ab8ff;border-color:#9ab8ff}.ops-actions button:disabled{display:none}.ops-actions button.primary:disabled{background:transparent;border-color:#3b4b60;color:#7f8da0;opacity:1}
.ops-metrics{display:grid;grid-template-columns:repeat(5,minmax(0,1fr));border-top:1px solid #2b3a4d;border-bottom:1px solid #2b3a4d}
.ops-metrics>div{display:flex;flex-direction:column;gap:2px;min-width:0;padding:var(--s-3) var(--s-4);border-right:1px solid #2b3a4d}.ops-metrics>div:first-child{padding-left:0}.ops-metrics>div:last-child{border-right:0}.metric-value{font-size:1.35rem;line-height:1.2;font-weight:620;font-variant-numeric:tabular-nums;color:var(--ops-text)}.metric-label{color:var(--ops-muted);font-size:var(--t-xs)}
.pipeline-track{display:grid;grid-template-columns:repeat(7,minmax(0,1fr));gap:1px;list-style:none;margin:var(--s-4) 0 0;padding:0;border:1px solid #2b3a4d;border-radius:var(--r-sm);overflow:hidden;background:#2b3a4d}.pipeline-stage{display:grid;grid-template-columns:auto 1fr auto;align-items:center;gap:var(--s-2);min-height:42px;padding:0 var(--s-3);background:#18212d;color:var(--ops-muted)}.pipeline-stage.is-active{background:#213354;color:#dce7ff}.pipeline-index{font-family:var(--font-mono);font-size:.625rem;color:#72839a}.pipeline-name{font-size:var(--t-xs);font-weight:550;white-space:nowrap}.pipeline-count{font-family:var(--font-mono);font-size:var(--t-xs);font-variant-numeric:tabular-nums}.pipeline-stage.is-active .pipeline-count{color:#a9c1ff}
.ops-foot{display:flex;flex-wrap:wrap;gap:var(--s-1) var(--s-2);margin:var(--s-3) 0 0;color:var(--ops-muted);font-size:var(--t-xs)}
@media (prefers-color-scheme:dark){.ops-overview{background:#111821;border-color:#263444}.ops-metrics,.pipeline-track{border-color:#283545}.pipeline-stage{background:#111821}.pipeline-stage.is-active{background:#17243a}}

.task-composer{background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);margin:0 0 var(--s-4);overflow:hidden}
.task-composer>summary{display:grid;grid-template-columns:32px minmax(0,1fr) auto;align-items:center;gap:var(--s-3);padding:var(--s-4);cursor:pointer;list-style:none;outline:none}.task-composer>summary::-webkit-details-marker{display:none}.task-composer>summary:hover{background:var(--surface-2)}.task-composer>summary:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
.composer-mark{position:relative;width:32px;height:32px;border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface-2)}.composer-mark::before,.composer-mark::after{content:"";position:absolute;left:50%;top:50%;width:12px;height:1.5px;background:var(--fg-muted);transform:translate(-50%,-50%)}.composer-mark::after{transform:translate(-50%,-50%) rotate(90deg)}.task-composer[open] .composer-mark::after{transform:translate(-50%,-50%) rotate(0)}
.composer-copy{display:flex;flex-direction:column;gap:2px;min-width:0}.composer-copy strong{font-size:var(--t-md);font-weight:600}.composer-copy small{color:var(--fg-muted);font-size:var(--t-sm)}.composer-toggle{color:var(--fg-muted);font-size:var(--t-xs);white-space:nowrap}.composer-body{border-top:1px solid var(--line);padding:var(--s-4)}
.composer-grid{display:grid;grid-template-columns:minmax(0,1.25fr) minmax(260px,.75fr);gap:var(--s-3)}.composer-section,.composer-group{border:1px solid var(--line);border-radius:var(--r-md);background:var(--surface-2);padding:var(--s-4)}.composer-section-primary{border-color:color-mix(in srgb,var(--accent) 28%,var(--line))}.section-badge,.section-count{display:inline-flex;align-items:center;min-height:22px;padding:0 8px;border-radius:var(--r-pill);background:var(--accent-soft);color:var(--accent);font-size:var(--t-xs);font-weight:600;white-space:nowrap}.section-count{background:var(--sunken);color:var(--fg-muted);font-variant-numeric:tabular-nums}.field-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:var(--s-3)}.field-grid>label{margin:0}.composer-options{grid-template-columns:repeat(3,minmax(0,1fr))}.composer-options label{align-self:end}.action-row{justify-content:flex-start}.form-note{color:var(--fg-subtle);font-size:var(--t-xs)}
.saved-list{display:grid;gap:1px;border:1px solid var(--line);border-radius:var(--r-sm);overflow:hidden;background:var(--line)}.saved-playlist{display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);padding:10px 12px;background:var(--surface);min-width:0}.saved-playlist>div:first-child{display:flex;flex-direction:column;gap:2px;min-width:0}.saved-playlist strong{font-size:var(--t-sm);font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.saved-playlist span{color:var(--fg-muted);font-size:var(--t-xs)}.saved-playlist-id{font-family:var(--font-mono);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;max-width:12rem}.inline-actions{display:flex;gap:var(--s-1);flex:0 0 auto}.inline-empty{padding:var(--s-4);background:var(--surface);color:var(--fg-muted);font-size:var(--t-sm);text-align:center}
.composer-group{grid-column:1/-1;padding:0}.composer-group>summary{display:flex;align-items:center;justify-content:space-between;padding:var(--s-3) var(--s-4);cursor:pointer;list-style:none}.composer-group>summary::-webkit-details-marker{display:none}.composer-group>summary:hover{background:var(--surface)}.composer-group>summary span:first-child{display:flex;flex-direction:column;gap:2px}.composer-group>summary strong{font-size:var(--t-sm);font-weight:600}.composer-group>summary small{font-size:var(--t-xs);color:var(--fg-muted)}.summary-disclosure{width:8px;height:8px;border-right:1.5px solid var(--fg-muted);border-bottom:1.5px solid var(--fg-muted);transform:rotate(45deg);margin-right:3px}.composer-group[open] .summary-disclosure{transform:rotate(225deg)}.composer-group-body{border-top:1px solid var(--line);padding:var(--s-4)}

.task-filter-bar{position:sticky;top:61px;z-index:20;display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);margin:0 0 var(--s-3);padding:var(--s-2) 0;background:var(--bg)}.task-filters{display:flex;align-items:center;gap:var(--s-1);min-width:0;overflow-x:auto;scrollbar-width:none}.task-filters::-webkit-scrollbar{display:none}.task-filters button{min-height:30px;padding:3px 9px;border-color:transparent;background:transparent;color:var(--fg-muted);white-space:nowrap}.task-filters button:hover{background:var(--surface);border-color:var(--line)}.task-filters button[aria-pressed=true]{background:var(--accent-soft);border-color:color-mix(in srgb,var(--accent) 24%,var(--line));color:var(--accent)}.task-filters button span{margin-left:5px;color:inherit;font-size:var(--t-xs);font-variant-numeric:tabular-nums;opacity:.72}.loaded-count{color:var(--fg-subtle);font-size:var(--t-xs);white-space:nowrap}
.batch{display:grid;grid-template-columns:auto minmax(0,1fr) auto;align-items:center;gap:var(--s-3);margin:0 0 var(--s-3);padding:var(--s-2) var(--s-3);border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface)}.batch-count{color:var(--fg-muted);font-size:var(--t-xs);font-variant-numeric:tabular-nums;white-space:nowrap}.batch-selection,.batch-actions{display:flex;flex-wrap:wrap;gap:var(--s-1)}.batch-selection{justify-self:start}.batch-actions{justify-self:end}.batch button{min-height:29px;padding:3px 8px}.batch button[data-batch-select]{background:transparent;border-color:transparent;color:var(--fg-muted)}.batch button[data-batch-select]:hover{background:var(--surface-2);border-color:var(--line);color:var(--fg)}.batch button[data-batch-action=resume],.batch button[data-batch-action=retry]{background:var(--accent-soft);border-color:color-mix(in srgb,var(--accent) 24%,var(--line));color:var(--accent)}
.task-list{display:block;background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);overflow:hidden}.task-item{display:grid!important;grid-template-columns:40px minmax(0,1fr);padding:0!important;border:0!important;border-left:2px solid transparent!important;border-bottom:1px solid var(--line)!important;border-radius:0!important;background:transparent!important;transition:background var(--dur) var(--ease),border-color var(--dur) var(--ease)}.task-item:last-child{border-bottom:0!important}.task-item:hover{background:var(--surface-2)!important;border-left-color:var(--line-strong)!important}.task-item.is-active{border-left-color:var(--accent)!important}.task-item.is-review{border-left-color:var(--warn)!important}.task-item.is-failed{border-left-color:var(--danger)!important}.task-select{position:relative;display:flex!important;align-items:center!important;justify-content:center;align-self:stretch;margin:0!important;border-right:1px solid var(--line);background:var(--surface-2)}.task-select input{position:absolute;opacity:0}.task-select span{display:block;width:16px;height:16px;border:1px solid var(--line-strong);border-radius:4px;background:var(--surface)}.task-select input:checked+span{background:var(--accent);border-color:var(--accent)}.task-select input:checked+span::after{content:"";display:block;width:4px;height:8px;margin:1px 0 0 4px;border-right:2px solid var(--accent-fg);border-bottom:2px solid var(--accent-fg);transform:rotate(45deg)}.task-select input:focus-visible+span{box-shadow:var(--ring)}.task-select.placeholder{background:var(--surface)}.task-item .card-link{display:grid!important;grid-template-columns:minmax(0,1fr) auto!important;align-items:center!important;gap:var(--s-3);min-height:76px;padding:12px 16px!important;border:0!important;background:transparent!important;text-align:left}.task-main{display:grid;gap:4px;min-width:0}.task-top{display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);min-width:0}.task-title{font-size:var(--t-md);font-weight:600;color:var(--fg);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.task-status{display:flex;align-items:center;gap:var(--s-1);flex:0 0 auto}.task-exec{color:var(--fg-subtle);font-size:var(--t-xs);white-space:nowrap}.task-summary{color:var(--fg-muted);font-size:var(--t-sm);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.task-foot{display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);color:var(--fg-subtle);font-size:var(--t-xs)}.task-caret{color:var(--fg-subtle);font-size:1.25rem;line-height:1}.task-list .card-link:hover{background:transparent!important}.review-summary{display:flex;align-items:center;justify-content:space-between;gap:var(--s-4);padding:var(--s-4) var(--s-5);margin:0 0 var(--s-3);border:1px solid var(--line);border-left:2px solid var(--accent);border-radius:var(--r-md);background:var(--surface)}.review-summary h2{margin:2px 0;font-size:var(--t-xl)}.review-summary p{margin:0;color:var(--fg-muted);font-size:var(--t-sm)}.section-kicker{color:var(--accent);font-size:var(--t-xs);font-weight:600}.empty-state{padding:var(--s-6) var(--s-4);border:1px dashed var(--line-strong);border-radius:var(--r-md);background:color-mix(in srgb,var(--surface) 72%,transparent);text-align:center}.empty-state h2{font-size:var(--t-lg)}.empty-state p{margin:0 auto var(--s-3);max-width:38rem;color:var(--fg-muted)}.pagination{display:flex;align-items:center;justify-content:space-between;gap:var(--s-3);padding:var(--s-3) 0;color:var(--fg-muted);font-size:var(--t-xs)}/* ---------- responsive ---------- */
@media (max-width:900px){
  .composer-grid{grid-template-columns:minmax(0,1fr)}
  .ops-main{grid-template-columns:auto minmax(0,1fr)}
  .ops-actions{grid-column:1/-1;justify-content:flex-end}
}
@media (max-width:720px){
  .app-bar-inner{grid-template-columns:minmax(0,1fr);gap:0;padding:var(--s-3) var(--s-4) 0}
  .mark{padding-bottom:var(--s-2)}
  nav{margin:0 calc(var(--s-4) * -1);padding:0 var(--s-4)}
  main{padding:var(--s-4) var(--s-4) var(--s-6)}
  .view-head{align-items:flex-start;flex-wrap:wrap}
  h1{font-size:1.5rem}
  .view-live{min-height:28px;margin-left:auto;padding:0 10px}
  .ops-overview{padding:var(--s-4)}
  .ops-main{grid-template-columns:minmax(0,1fr);gap:var(--s-3)}
  .ops-actions{grid-column:auto;justify-content:flex-start}
  .ops-metrics{grid-template-columns:repeat(3,minmax(0,1fr))}
  .ops-metrics>div{padding:9px 10px;border-right:1px solid #2b3a4d;border-bottom:1px solid #2b3a4d}.ops-metrics>div:first-child{padding-left:10px}.ops-metrics>div:nth-child(3){border-right:0}.ops-metrics>div:nth-child(4),.ops-metrics>div:nth-child(5){border-bottom:0}.ops-metrics>div:nth-child(4){padding-left:0}.ops-metrics>div:nth-child(5){border-right:0}
  .pipeline-track{display:flex;overflow-x:auto;scrollbar-width:none}.pipeline-track::-webkit-scrollbar{display:none}.pipeline-stage{flex:0 0 7.25rem}
  .task-composer>summary{grid-template-columns:32px minmax(0,1fr);padding:var(--s-3)}
  .composer-copy small{white-space:normal;overflow-wrap:anywhere}
  .composer-body{padding:var(--s-3)}
  .field-grid,.composer-options{grid-template-columns:minmax(0,1fr)}
  .saved-playlist{align-items:flex-start;flex-direction:column}
  .saved-playlist-id{max-width:100%}
  .task-filter-bar{top:108px;align-items:flex-start;flex-direction:column;gap:var(--s-1);padding:var(--s-2) 0}
  .task-filters{width:100%}
  .loaded-count{padding-left:2px}
  .batch{grid-template-columns:minmax(0,1fr);gap:var(--s-2)}
  .batch-count,.batch-selection,.batch-actions{justify-self:start}
  .batch-actions{width:100%;overflow-x:auto}
  .cards{grid-template-columns:minmax(0,1fr)}
  dl{grid-template-columns:minmax(0,1fr);gap:var(--s-1)}
  dt{font-size:var(--t-xs)}
  .selection-toolbar{align-items:flex-start;flex-direction:column}
  .task-item{grid-template-columns:34px minmax(0,1fr)}
  .task-item .card-link{padding:11px 12px!important}
  .task-top{align-items:flex-start;flex-direction:column;gap:4px}
  .task-title{white-space:normal}
  .task-summary{white-space:normal;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical}
  .task-status{flex-wrap:wrap}
  .review-summary{align-items:flex-start;flex-direction:column;padding:var(--s-4)}
  #detail{padding:var(--s-4)}
}
@media (max-width:430px){
  .composer-toggle{display:none}
  .ops-actions{display:grid;grid-template-columns:1fr 1fr}
  .view-head{gap:var(--s-2)}
  .view-subtitle{max-width:18rem}
  .task-select.placeholder{display:none!important}.task-item:has(.task-select.placeholder){grid-template-columns:minmax(0,1fr)}
  .task-item{grid-template-columns:34px minmax(0,1fr)}
}
@media (prefers-reduced-motion:reduce){
  *,*::before,*::after{transition:none!important;animation:none!important}
}
`
