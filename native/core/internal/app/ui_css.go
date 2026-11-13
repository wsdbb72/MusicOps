package app

// appCSS is the design system for the local UI. Tokens come first, then the
// shell, then components: every colour and spacing step is read from a token,
// so the dark scheme is a token swap instead of a second stylesheet.
//
// The rules here are intentionally scoped to the elements the page actually
// builds (see appJS and the shell in http.go). Anything a component needs is
// declared once; nothing relies on inheritance from a parent card.
const appCSS = `/* ---------- tokens ---------- */
:root{
  color-scheme:light;
  --font-ui:"Segoe UI Variable Text","Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei","Noto Sans SC",system-ui,sans-serif;
  --font-mono:"Cascadia Mono","SFMono-Regular",Consolas,"Liberation Mono",monospace;
  --t-xs:.78rem;--t-sm:.86rem;--t-md:.95rem;--t-lg:1.06rem;--t-xl:1.3rem;--t-2xl:1.62rem;
  --lh-tight:1.28;--lh-base:1.55;
  --s-1:.25rem;--s-2:.5rem;--s-3:.75rem;--s-4:1rem;--s-5:1.5rem;--s-6:2rem;--s-7:3rem;
  --r-xs:6px;--r-sm:9px;--r-md:12px;--r-lg:16px;--r-pill:999px;
  --bg:#f4f2ed;--surface:#fff;--surface-2:#fbf9f5;--sunken:#eeebe4;
  --fg:#1b1a17;--fg-muted:#5b564d;--fg-subtle:#847d71;
  --line:#e3ddd2;--line-strong:#cfc7b8;
  --accent:#0f6e56;--accent-hover:#0b5a46;--accent-fg:#fff;--accent-soft:#e2f1eb;
  --warn:#8a4b08;--warn-soft:#fbead2;
  --info:#1d4e89;--info-soft:#e3edf9;
  --danger:#a32020;--danger-soft:#fbe7e5;
  --ring:0 0 0 2px var(--bg),0 0 0 4px rgba(15,110,86,.5);
  --shadow-1:0 1px 1px rgba(28,25,20,.04),0 2px 8px rgba(28,25,20,.05);
  --shadow-2:0 10px 30px rgba(28,25,20,.12);
  --dur:120ms;--ease:cubic-bezier(.2,.6,.25,1);
}
@media (prefers-color-scheme:dark){
  :root{
    color-scheme:dark;
    --bg:#141412;--surface:#1c1c19;--surface-2:#21211d;--sunken:#101010;
    --fg:#f0ede6;--fg-muted:#b4aea2;--fg-subtle:#8e887c;
    --line:#302e28;--line-strong:#443f36;
    --accent:#4cc2a0;--accent-hover:#63d2b2;--accent-fg:#06231b;--accent-soft:#12312a;
    --warn:#e0a45c;--warn-soft:#33230f;
    --info:#7fb0e8;--info-soft:#14243a;
    --danger:#f08b85;--danger-soft:#3a1a18;
    --ring:0 0 0 2px var(--bg),0 0 0 4px rgba(76,194,160,.55);
    --shadow-1:0 1px 2px rgba(0,0,0,.5);
    --shadow-2:0 12px 34px rgba(0,0,0,.6);
  }
}

/* ---------- base ---------- */
*,*::before,*::after{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;font-family:var(--font-ui);font-size:var(--t-md);line-height:var(--lh-base);color:var(--fg);background:var(--bg);-webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility}
h1,h2,h3{margin:0 0 var(--s-2);font-weight:650;line-height:var(--lh-tight);letter-spacing:-.01em}
h1{font-size:var(--t-2xl)}
h2{font-size:var(--t-lg)}
h3{font-size:var(--t-md);color:var(--fg-muted);text-transform:none}
p{margin:0 0 var(--s-3)}
p:last-child{margin-bottom:0}
a{color:var(--accent);text-underline-offset:2px}
code,kbd,samp{font-family:var(--font-mono);font-size:.9em;background:var(--sunken);border-radius:var(--r-xs);padding:.08em .34em}
hr{border:0;border-top:1px solid var(--line);margin:var(--s-4) 0}
::selection{background:var(--accent-soft);color:var(--fg)}
:where(button,a,input,select,textarea,summary,[tabindex]):focus-visible{outline:none;box-shadow:var(--ring);border-radius:var(--r-sm)}
[hidden]{display:none!important}

/* ---------- shell ---------- */
.skip{position:absolute;left:var(--s-4);top:var(--s-4);z-index:60;transform:translateY(-240%);background:var(--surface);color:var(--fg);border:1px solid var(--line-strong);border-radius:var(--r-sm);padding:var(--s-2) var(--s-3);text-decoration:none;box-shadow:var(--shadow-2);transition:transform var(--dur) var(--ease)}
.skip:focus{transform:none}
.app{display:grid;grid-template-rows:auto minmax(0,1fr);min-height:100dvh}
.app-bar{display:flex;flex-direction:column;gap:var(--s-3);padding:var(--s-4) var(--s-5);border-bottom:1px solid var(--line);background:var(--surface);position:sticky;top:0;z-index:30}
.mark{margin:0;display:flex;align-items:center;gap:var(--s-2);color:var(--accent);font-weight:700;font-size:var(--t-md);letter-spacing:.06em;white-space:nowrap;min-width:0;overflow:hidden}
.mark span{flex:0 0 auto;white-space:nowrap}
.mark .brand-name{color:var(--accent);font-weight:700;font-size:var(--t-md);letter-spacing:.06em}
.mark .brand-sub{color:var(--fg-subtle);font-weight:500;font-size:var(--t-xs);letter-spacing:.02em}
.brand-icon {
  flex: 0 0 auto;
  width: 24px;
  height: 24px;
  margin-right: 2px;
  border-radius: 6px;
}

/* On a narrow window the subtitle is the first thing to go; the product
   name and the version stay. */
@media (max-width: 640px) {
  .mark .brand-sub { display: none; }
}
.mark .app-version{color:var(--accent);background:var(--accent-soft);border-radius:var(--r-pill);padding:.08rem .5rem;font-family:var(--font-mono,ui-monospace,SFMono-Regular,Menlo,monospace);font-weight:650;font-size:var(--t-xs);letter-spacing:.02em}

.pick-list{display:grid;gap:2px;max-height:22rem;overflow:auto;border:1px solid var(--line);border-radius:var(--r-md);padding:.4rem .6rem;margin:.4rem 0}
.pick-row{display:flex;align-items:center;gap:.4rem;font-size:var(--t-sm);padding:.18rem 0}
.pick-row input{flex:0 0 auto}

/* ---------- navigation ---------- */
nav{display:flex;gap:var(--s-1);overflow-x:auto;scrollbar-width:none;margin:0 calc(var(--s-5) * -1);padding:0 var(--s-5)}
nav::-webkit-scrollbar{display:none}
nav button{flex:0 0 auto;border:1px solid transparent;background:transparent;color:var(--fg-muted);border-radius:var(--r-pill);padding:.36rem .8rem;font-size:var(--t-sm);font-weight:550}
nav button:hover{background:var(--sunken);color:var(--fg)}
nav button[aria-current="page"]{background:var(--accent-soft);border-color:transparent;color:var(--accent);font-weight:650}
nav button:disabled{opacity:.5}

/* ---------- main ---------- */
main{display:block;width:100%;max-width:76rem;margin:0 auto;padding:var(--s-5) var(--s-5) var(--s-7)}
main:focus{outline:none}
.view-head{display:flex;align-items:baseline;justify-content:space-between;gap:var(--s-3);margin-bottom:var(--s-2)}
.view-head h1{margin:0}
#status{display:flex;align-items:center;gap:var(--s-2);min-height:1.7rem;margin:0 0 var(--s-4);color:var(--fg-muted);font-size:var(--t-sm)}
#status:empty{display:none;min-height:0;margin:0}
#status::before{content:"";flex:0 0 auto;width:.45rem;height:.45rem;border-radius:50%;background:currentColor;opacity:.75}
#status.ok{color:var(--accent)}
#status.fail{color:var(--danger)}
#status.review{color:var(--warn)}
#status.info{color:var(--info)}

/* ---------- buttons ---------- */
button{font:inherit;font-size:var(--t-sm);font-weight:550;display:inline-flex;align-items:center;justify-content:center;gap:var(--s-2);border:1px solid var(--line-strong);background:var(--surface);color:var(--fg);border-radius:var(--r-sm);padding:.4rem .8rem;cursor:pointer;transition:background var(--dur) var(--ease),border-color var(--dur) var(--ease),color var(--dur) var(--ease),transform var(--dur) var(--ease)}
button:hover{background:var(--sunken)}
button:active{transform:translateY(1px)}
button:disabled{opacity:.5;cursor:not-allowed;transform:none}
button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}
button.primary:hover{background:var(--accent-hover);border-color:var(--accent-hover)}
button.danger{color:var(--danger);border-color:rgba(163,32,32,.4);background:var(--danger-soft)}
button.danger:hover{background:var(--danger-soft);border-color:var(--danger)}
button[aria-current="page"]{background:var(--accent);border-color:var(--accent);color:var(--accent-fg)}

/* ---------- forms ---------- */
form{margin:0}
label{display:block;margin:0 0 var(--s-3);color:var(--fg-muted);font-size:var(--t-sm)}
label>input,label>select,label>textarea{margin-top:var(--s-1)}
input[type="text"],input[type="password"],input[type="url"],input[type="number"],input[type="search"],select,textarea{width:100%;padding:.44rem .62rem;border:1px solid var(--line-strong);border-radius:var(--r-sm);font:inherit;font-size:var(--t-md);color:var(--fg);background:var(--surface);transition:border-color var(--dur) var(--ease)}
input[type="text"]:hover,input[type="password"]:hover,input[type="url"]:hover,input[type="number"]:hover,select:hover,textarea:hover{border-color:var(--fg-subtle)}
input[type="text"]:focus,input[type="password"]:focus,input[type="url"]:focus,input[type="number"]:focus,select:focus,textarea:focus{outline:none;border-color:var(--accent);box-shadow:var(--ring)}
textarea{min-height:6rem;resize:vertical}
select{appearance:none;padding-right:1.9rem;background-image:linear-gradient(45deg,transparent 50%,var(--fg-subtle) 50%),linear-gradient(135deg,var(--fg-subtle) 50%,transparent 50%);background-position:calc(100% - 1.05rem) 52%,calc(100% - .7rem) 52%;background-size:.35rem .35rem,.35rem .35rem;background-repeat:no-repeat}
label:has(> input[type="checkbox"]),label:has(> input[type="radio"]){display:flex;align-items:flex-start;gap:var(--s-2);color:var(--fg);font-size:var(--t-md);line-height:1.45}
input[type="checkbox"],input[type="radio"]{flex:0 0 auto;width:1rem;height:1rem;margin:.18rem 0 0;accent-color:var(--accent)}
fieldset{border:1px solid var(--line);border-radius:var(--r-md);padding:var(--s-3) var(--s-4) var(--s-2);margin:0 0 var(--s-3);background:var(--surface-2)}
legend{padding:0 var(--s-2);color:var(--fg-muted);font-size:var(--t-sm);font-weight:600}
fieldset label:last-child{margin-bottom:var(--s-2)}
#search-sites label{display:inline-flex;margin-right:var(--s-3)}
#search-sites br{display:none}

/* ---------- cards ---------- */
.cards{display:grid;gap:var(--s-3);min-width:0}
@media (min-width:52rem){.cards{grid-template-columns:repeat(auto-fill,minmax(27rem,1fr));align-items:start}}
article{display:flex;flex-direction:column;min-width:0;background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);padding:var(--s-4);box-shadow:var(--shadow-1);transition:border-color var(--dur) var(--ease),box-shadow var(--dur) var(--ease)}
article:has(.card-link):hover{border-color:var(--line-strong);box-shadow:var(--shadow-2)}
article>h2:first-child{margin-top:0}
.card-link{display:block;width:100%;border:0;background:transparent;padding:0;text-align:left;color:inherit;font:inherit;border-radius:var(--r-sm);justify-content:flex-start}
.card-link:hover{background:transparent}
.card-link:active{transform:none}
.card-link h2{margin:0 0 var(--s-1);color:var(--fg);font-size:var(--t-lg);font-weight:650}
.card-link p{margin:0;color:var(--fg-muted);font-size:var(--t-sm)}
.card-link p:first-of-type{margin-bottom:var(--s-1)}

/* ---------- meta, pills, batches ---------- */
.meta{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-1) var(--s-2);margin:var(--s-2) 0;color:var(--fg-muted);font-size:var(--t-sm)}
.meta:empty{display:none}
.pill{display:inline-flex;align-items:center;gap:.3rem;border-radius:var(--r-pill);padding:.06rem .55rem;background:var(--sunken);color:var(--fg-muted);font-size:var(--t-xs);font-weight:550;white-space:nowrap}
.pill.review{background:var(--warn-soft);color:var(--warn)}
.pill.queue{background:var(--info-soft);color:var(--info)}
.batch{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-2);margin:0 0 var(--s-3);padding:var(--s-2) var(--s-3);border:1px solid var(--line);border-radius:var(--r-md);background:var(--surface-2)}
.batch>span{color:var(--fg-muted);font-size:var(--t-sm);margin-right:var(--s-1)}

/* ---------- detail ---------- */
#detail{background:var(--surface);border:1px solid var(--line);border-radius:var(--r-lg);padding:var(--s-4) var(--s-5) var(--s-5);box-shadow:var(--shadow-1)}
#back{margin-bottom:var(--s-3)}
#detail-title{font-size:var(--t-xl);margin:0 0 var(--s-2)}
dl{display:grid;grid-template-columns:minmax(4.5rem,7.5rem) minmax(0,1fr);gap:var(--s-2) var(--s-4);margin:var(--s-4) 0;align-items:baseline}
dt{color:var(--fg-subtle);font-size:var(--t-sm)}
dd{margin:0;color:var(--fg);overflow-wrap:anywhere}
#detail-actions,#torrent-actions{display:flex;flex-wrap:wrap;gap:var(--s-2);margin:var(--s-3) 0}
#torrent-actions{padding-top:var(--s-3);border-top:1px dashed var(--line)}
.review-box{margin-top:var(--s-4);border:1px solid var(--line);border-left:3px solid var(--warn);border-radius:var(--r-md);padding:var(--s-4);background:var(--surface-2)}
.review-box h3{margin-top:0;color:var(--warn)}
.review-box .candidate{display:flex;align-items:flex-start;gap:var(--s-2);margin:0 0 var(--s-2);padding:var(--s-2) var(--s-3);border:1px solid var(--line);border-radius:var(--r-sm);background:var(--surface);cursor:pointer;transition:border-color var(--dur) var(--ease),background var(--dur) var(--ease)}
.review-box .candidate:hover{border-color:var(--line-strong)}
.review-box .candidate:has(input:checked){border-color:var(--accent);background:var(--accent-soft)}
#review-actions{display:flex;flex-wrap:wrap;align-items:center;gap:var(--s-2);margin:var(--s-3) 0 0}

/* ---------- notes and library ---------- */
#note{border:1px dashed var(--line-strong);border-radius:var(--r-md);padding:var(--s-4);color:var(--fg-muted);background:var(--surface-2)}
.library-track{font-family:var(--font-mono);font-size:var(--t-xs);color:var(--fg-muted)}
#list{min-width:0}

/* ---------- responsive ---------- */
@media (min-width:67.5rem){
  .app{grid-template-rows:minmax(0,1fr);grid-template-columns:15.5rem minmax(0,1fr)}
  .app-bar{grid-column:1;grid-row:1;align-self:start;position:sticky;top:0;height:100dvh;border-bottom:0;border-right:1px solid var(--line);gap:var(--s-5)}
  nav{flex-direction:column;margin:0;padding:0;overflow:visible}
  nav button{width:100%;justify-content:flex-start;border-radius:var(--r-sm);padding:.5rem .7rem;text-align:left}
  main{grid-column:2;grid-row:1;padding:var(--s-6) var(--s-6) var(--s-7)}
}
@media (max-width:720px){
  header,main{padding-left:var(--s-4);padding-right:var(--s-4)}
  nav{margin:0 calc(var(--s-4) * -1);padding:0 var(--s-4)}
  h1{font-size:var(--t-xl)}
  .cards{grid-template-columns:minmax(0,1fr)}
  dl{grid-template-columns:minmax(0,1fr);gap:var(--s-1)}
  dt{font-size:var(--t-xs);text-transform:uppercase;letter-spacing:.04em}
  #detail{padding:var(--s-4)}
  .app-bar{padding:var(--s-3) var(--s-4)}
}
@media (prefers-reduced-motion:reduce){
  *,*::before,*::after{transition:none!important;animation:none!important}
}
`
