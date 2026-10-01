// app.js —— 状态轮询 + 渲染（无构建步骤：纯静态文件，Wails 直接嵌入）
const $ = (id) => document.getElementById(id);
let lastState = null;

async function poll() {
  let s;
  try {
    s = await window.go.gui.Bridge.Status();
  } catch (e) {
    setDot("err", "桥接不可用: " + e);
    return;
  }
  lastState = s;
  render(s);
}

function setDot(cls, text) {
  $("dot").className = "dot " + cls;
  $("stateText").textContent = text;
}

function render(s) {
  // 头部状态
  if (s.error) {
    setDot("err", "启动失败");
    $("errBanner").textContent = "启动失败: " + s.error;
    $("errBanner").classList.remove("hidden");
  } else if (!s.running) {
    setDot("starting", "正在启动同步引擎…");
  } else {
    const busy = (s.stats.inflight || 0) + (s.stats.pending || 0) > 0;
    setDot(busy ? "busy" : "ok", busy ? "正在同步" : "已是最新");
  }
  $("target").textContent = s.remote
    ? `${s.root} ⇄ ${s.remote}${s.offline ? "（离线模式）" : ""}`
    : s.root;

  // 概览
  $("nPending").textContent = s.stats.pending || 0;
  $("nInflight").textContent = s.stats.inflight || 0;
  $("nFailed").textContent = s.stats.failed || 0;
  $("nDone").textContent = s.stats.done || 0;

  // 认证停摆横幅
  $("authBanner").classList.toggle("hidden", !s.authFailed);

  // 动作列表（OneDrive 传输视图）
  const list = $("list");
  list.textContent = "";
  if (!s.actions || s.actions.length === 0) {
    const d = document.createElement("div");
    d.className = "empty";
    d.textContent = s.running ? "没有正在进行的传输" : "…";
    list.appendChild(d);
  } else {
    for (const a of s.actions) {
      const row = document.createElement("div");
      row.className = "row";
      const ico = a.state === "inflight" ? "⏳" : "…";
      const meta = a.lastErr ? `第 ${a.attempts + 1} 次尝试 · ${a.lastErr}` : `第 ${a.attempts + 1} 次尝试`;
      row.innerHTML =
        `<span class="ico">${ico}</span>` +
        `<span class="body"><div class="path" title="${esc(a.path)}">${esc(a.path)}</div>` +
        `<div class="meta">${esc(meta)}</div></span>` +
        `<span class="kind ${a.kind}"></span>`;
      list.appendChild(row);
    }
  }
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

$("btnRoot").onclick = () => window.go.gui.Bridge.OpenRoot().catch(alert);
$("btnLog").onclick = () => window.go.gui.Bridge.OpenLog().catch(alert);
$("btnQuit").onclick = () => window.go.gui.Bridge.Quit().catch(alert);
$("btnRetry").onclick = async () => {
  try {
    const n = await window.go.gui.Bridge.RetryFailed();
    poll();
  } catch (e) { alert(e); }
};

poll();
setInterval(poll, 2000);
