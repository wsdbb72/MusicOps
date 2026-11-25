package app

// appCSS is the local MusicOps design system. The visual direction is a
// compact library control desk: quiet neutral chrome, one teal operational
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
  --bg:#f5f6f7;--surface:#fff;--surface-2:#f1f2f4;--sunken:#e9ebee;
  --fg:#17191d;--fg-muted:#5d6470;--fg-subtle:#747c88;
  --line:#e2e4e8;--line-strong:#cdd2d9;
  --accent:#166b5d;--accent-hover:#10594d;--accent-fg:#fff;--accent-soft:#e6f1ee;
  --warn:#875a12;--warn-soft:#f7eedc;
  --info:#365f93;--info-soft:#e9eff7;
  --danger:#a32a2a;--danger-soft:#faeaea;
  --ring:0 0 0 2px var(--surface),0 0 0 4px rgba(22,107,93,.42);
  --shadow-1:0 1px 2px rgba(18,22,28,.04);
  --shadow-2:0 12px 32px rgba(18,22,28,.12);
  --dur:150ms;--ease:cubic-bezier(.22,1,.36,1);
}
@media (prefers-color-scheme:dark){
  :root{
    color-scheme:dark;
    --bg:#0f1114;--surface:#17191d;--surface-2:#1c1f24;--sunken:#22262c;
    --fg:#eef0f3;--fg-muted:#aeb4bd;--fg-subtle:#858d98;
    --line:#2a2e35;--line-strong:#3d434c;
    --accent:#62c3af;--accent-hover:#78d0be;--accent-fg:#09231d;--accent-soft:#16332d;
    --warn:#e1ad62;--warn-soft:#332716;
    --info:#8bb5e8;--info-soft:#19283c;
    --danger:#ed8c88;--danger-soft:#3a2020;
    --ring:0 0 0 2px var(--surface),0 0 0 4px rgba(98,195,175,.45);
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
.app{display:grid;grid-template-rows:auto minmax(0,1fr);min-height:100dvh}
.app-bar{display:grid;grid-template-columns:auto minmax(0,1fr);align-items:end;gap:var(--s-4) var(--s-5);padding:var(--s-3) var(--s-5) 0;border-bottom:1px solid var(--line);background:var(--surface);position:sticky;top:0;z-index:30}
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
main{display:block;width:100%;max-width:72rem;margin:0 auto;padding:var(--s-5) var(--s-5) var(--s-7)}
main:focus{outline:none}
.view-head{display:flex;align-items:baseline;justify-content:space-between;gap:var(--s-3);margin-bottom:var(--s-1)}
.view-head h1{margin:0;font-size:var(--t-xl);font-weight:600}
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

/* ---------- responsive ---------- */
@media (max-width:720px){
  .app-bar{grid-template-columns:1fr;gap:0;padding:var(--s-3) var(--s-4) 0}
  .mark{padding-bottom:var(--s-2)}
  nav{margin:0 calc(var(--s-4) * -1);padding:0 var(--s-4)}
  main{padding:var(--s-4) var(--s-4) var(--s-6)}
  h1{font-size:var(--t-xl)}
  .cards{grid-template-columns:minmax(0,1fr)}
  dl{grid-template-columns:minmax(0,1fr);gap:var(--s-1)}
  dt{font-size:var(--t-xs)}
  .selection-toolbar{align-items:flex-start;flex-direction:column}
  #detail{padding:var(--s-4)}
}
@media (prefers-reduced-motion:reduce){
  *,*::before,*::after{transition:none!important;animation:none!important}
}
`
