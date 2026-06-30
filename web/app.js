// --- 探针前端: 服务器列表 / 详情 / 实时图表 ---
const app = document.getElementById("app");
let states = {};      // agentID -> latest state
let selected = null;  // current agentID in detail view
let ws = null;
const charts = {};    // agentID -> {cpu, mem, net}

const fmt = {
  bytes(n) {
    if (!n && n !== 0) return "—";
    const u = ["B","KB","MB","GB","TB","PB"];
    let i = 0; let v = n;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return v.toFixed(v >= 100 ? 0 : 1) + " " + u[i];
  },
  rate(n) { return fmt.bytes(n) + "/s"; },
  pct(a, b) { if (!b) return "0%"; return Math.min(100, (a / b) * 100).toFixed(0) + "%"; },
  uptime(s) {
    if (!s) return "—";
    const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    if (d > 0) return d + "天" + h + "小时";
    if (h > 0) return h + "小时" + m + "分";
    return m + "分";
  },
  time(ts) {
    if (!ts) return "—";
    const d = new Date(ts * 1000);
    return d.toLocaleTimeString("zh-CN");
  },
};

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  // If the page was opened with ?token=..., forward it on the WS handshake so
  // the dashboard can authorize the viewer (only needed when a web-token is set).
  const tok = new URLSearchParams(location.search).get("token");
  const wsPath = tok ? "/ws?token=" + encodeURIComponent(tok) : "/ws";
  ws = new WebSocket(proto + "//" + location.host + wsPath);
  ws.onopen = () => { document.querySelector("header .dot").style.background = "var(--green)"; };
  ws.onclose = () => {
    document.querySelector("header .dot").style.background = "var(--red)";
    setTimeout(connect, 2000);
  };
  ws.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.type === "state") {
      states[msg.data.agent_id] = msg.data;
      if (selected === msg.data.agent_id) updateDetail();
      else renderList();
    }
  };
}

function route() {
  const id = location.hash.replace("#/", "");
  if (id && states[id]) {
    selected = id;
    renderDetail();
  } else {
    selected = null;
    renderList();
  }
}
window.addEventListener("hashchange", route);

// ---------- 列表 ----------
function renderList() {
  const ids = Object.keys(states);
  document.querySelector("header .summary").textContent =
    `${ids.length} 台服务器 · ${ids.filter(id => isOnline(states[id])).length} 在线`;
  if (ids.length === 0) {
    app.innerHTML = `<div class="empty"><span class="spin"></span><p style="margin-top:12px">等待 Agent 连接…</p>
      <p style="color:var(--muted);font-size:12px;margin-top:8px">在服务器上运行 agent 即可接入监控</p></div>`;
    return;
  }
  let html = '<main><div class="grid">';
  for (const id of ids) html += card(states[id]);
  html += "</div></main>";
  app.innerHTML = html;
  for (const el of app.querySelectorAll(".card")) {
    el.onclick = () => { location.hash = "/" + el.dataset.id; };
  }
  app.querySelectorAll(".edit-btn-sm").forEach(btn => {
    btn.onclick = (e) => { e.stopPropagation(); renameAgent(btn.dataset.rename); };
  });
}

function card(s) {
  const online = isOnline(s);
  const memP = fmt.pct(s.memory_used, s.memory_total);
  const cpuP = fmt.pct(s.cpu_usage, 100);
  const diskP = fmt.pct(s.disk_used, s.disk_total);
  const swapP = fmt.pct(s.swap_used, s.swap_total);
  return `<div class="card" data-id="${s.agent_id}">
    <div class="head">
      <div>
        <div class="name-row"><span class="name">${esc(s.name)}</span><button class="edit-btn-sm" data-rename="${s.agent_id}" title="修改名称">✎</button></div>
        <div class="os">${esc(s.os||"")} · ${s.cpu_count||"?"} 核 · ${esc(s.arch||"")}</div>
      </div>
      <span class="status ${online ? "online" : "offline"}">${online ? "在线" : "离线"}</span>
    </div>
    <div class="meters">
      <div class="meter"><div class="label"><span>CPU</span><b>${cpuP}</b></div><div class="bar"><i class="cpu" style="width:${cpuP}"></i></div></div>
      <div class="meter"><div class="label"><span>内存</span><b>${memP}</b></div><div class="bar"><i class="mem" style="width:${memP}"></i></div></div>
      <div class="meter"><div class="label"><span>磁盘</span><b>${diskP}</b></div><div class="bar"><i class="disk" style="width:${diskP}"></i></div></div>
      <div class="meter"><div class="label"><span>负载</span><b>${(s.load1||0).toFixed(2)}</b></div><div class="bar"><i class="disk" style="width:${Math.min(100,(s.load1||0)/(s.cpu_count||1)*100)}%"></i></div></div>
    </div>
    <div class="detail-grid">
      <div>内存 <b>${fmt.bytes(s.memory_used)} / ${fmt.bytes(s.memory_total)}</b></div>
      <div>磁盘 <b>${fmt.bytes(s.disk_used)} / ${fmt.bytes(s.disk_total)}</b></div>
      <div>交换 <b>${fmt.bytes(s.swap_used)} / ${fmt.bytes(s.swap_total)}</b></div>
      <div>连接 <b>${s.conn_count||0}</b></div>
    </div>
    <div class="net">
      <span>↓ <b>${fmt.rate(s.net_speed_in)}</b></span>
      <span>↑ <b>${fmt.rate(s.net_speed_out)}</b></span>
      <span>运行 <b>${fmt.uptime(s.uptime)}</b></span>
      <span>总入 <b>${fmt.bytes(s.net_in)}</b></span>
      <span>总出 <b>${fmt.bytes(s.net_out)}</b></span>
    </div>
  </div>`;
}

function isOnline(s) {
  if (!s || !s.timestamp) return false;
  return Date.now() / 1000 - s.timestamp < 15;
}

// ---------- 详情 ----------
function renderDetail() {
  const s = states[selected];
  if (!s) { route(); return; }
  app.innerHTML = `
  <main>
    <a class="back" href="#/">← 返回列表</a>
    <div class="detail-head">
      <h1 id="titleName">${esc(s.name)}</h1><button class="edit-btn" id="renameBtn" title="修改名称">✎</button>
      <span class="os">${esc(s.os||"")} · ${esc(s.arch||"")} · ${s.cpu_count} 核 ${esc(s.cpu_model||"")}</span>
    </div>
    <div class="stat-row">
      <div class="stat"><div class="k">CPU 使用率</div><div class="v">${(s.cpu_usage||0).toFixed(1)}<small>%</small></div></div>
      <div class="stat"><div class="k">内存</div><div class="v">${fmt.bytes(s.memory_used)}<small> / ${fmt.bytes(s.memory_total)}</small></div></div>
      <div class="stat"><div class="k">交换</div><div class="v">${fmt.bytes(s.swap_used)}<small> / ${fmt.bytes(s.swap_total)}</small></div></div>
      <div class="stat"><div class="k">运行时间</div><div class="v" style="font-size:16px">${fmt.uptime(s.uptime)}</div></div>
    </div>
    <div class="charts">
      <div class="chart-box"><h3>CPU 使用率 (%)</h3><canvas id="cpuChart"></canvas></div>
      <div class="chart-box"><h3>内存 / 网络</h3><canvas id="netChart"></canvas></div>
    </div>
    <div class="section">
      <h2>磁盘</h2>
      <div class="chart-box"><table id="diskTable"><thead><tr><th>挂载点</th><th>设备</th><th>类型</th><th class="num">已用</th><th class="num">总量</th><th class="num">使用率</th></tr></thead><tbody></tbody></table></div>
    </div>
    <div class="section">
      <h2>网络接口</h2>
      <div class="chart-box"><table id="netTable"><thead><tr><th>接口</th><th>IPv4</th><th>IPv6</th><th>MAC</th></tr></thead><tbody></tbody></table></div>
    </div>
    <div class="section">
      <h2>进程 (按内存排序 Top 10)</h2>
      <div class="chart-box"><table id="procTable"><thead><tr><th class="num">PID</th><th>名称</th><th class="num">CPU%</th><th class="num">内存</th></tr></thead><tbody></tbody></table></div>
    </div>
  </main>`;
  initCharts();
  bindRename(selected);
  updateDetail();
  // 拉取历史
  loadHistory(selected);
}

function updateDetail() {
  const s = states[selected];
  if (!s || !document.getElementById("diskTable")) return;
  // 更新各表格
  fillTable("diskTable", (s.disks||[]).map(d => [
    d.mountpoint, d.device, d.fs_type,
    fmt.bytes(d.used), fmt.bytes(d.total), fmt.pct(d.used, d.total),
  ]));
  fillTable("netTable", (s.interfaces||[]).map(i => [i.name, i.ipv4||"—", i.ipv6||"—", i.mac||"—"]));
  fillTable("procTable", (s.processes||[]).map(p => [p.pid, p.name, p.cpu.toFixed(1)+"%", fmt.bytes(p.memory)]));
  // 推入图表点
  pushChart(s);
}

function fillTable(id, rows) {
  const tb = document.querySelector("#" + id + " tbody");
  if (!tb) return;
  // Build rows as DOM nodes so untrusted host data (process names, mount
  // points, interface names) can never inject HTML.
  tb.replaceChildren();
  for (const r of rows) {
    const tr = document.createElement("tr");
    for (const c of r) {
      const td = document.createElement("td");
      const text = String(c);
      if (/[0-9.%]$/.test(text.replace(/[^0-9.%BKMGT]/g, ""))) td.className = "num";
      td.textContent = text;
      tr.appendChild(td);
    }
    tb.appendChild(tr);
  }
}

// ---------- 图表 ----------
function initCharts() {
  charts[selected] = {
    cpu: makeChart("cpuChart", ["#2f81f7"], 100),
    net: makeChart("netChart", ["#a371f7", "#3fb950", "#d29922"]),
  };
}
function makeChart(id, colors, max) {
  const ctx = document.getElementById(id);
  if (!ctx) return null;
  return new Chart(ctx, {
    type: "line",
    data: { labels: [], datasets: colors.map(c => ({ borderColor: c, backgroundColor: c+"22", data: [], tension: .3, pointRadius: 0, borderWidth: 2, fill: true })) },
    options: {
      animation: false, responsive: true, maintainAspectRatio: false,
      plugins: { legend: { display: colors.length > 1, labels: { color: "#8b949e", boxWidth: 10 } } },
      scales: {
        x: { display: false },
        y: max ? { min: 0, max: max, ticks: { color: "#8b949e" }, grid: { color: "#21262d" } } : { beginAtZero: true, ticks: { color: "#8b949e" }, grid: { color: "#21262d" } },
      },
    },
  });
}
function pushChart(s) {
  const c = charts[selected];
  if (!c) return;
  const t = fmt.time(s.timestamp);
  push(c.cpu, [t, [s.cpu_usage]]);
  const memUsed = (s.memory_used / (s.memory_total||1)) * 100;
  push(c.net, [t, [memUsed, s.net_speed_in ? Math.log10(s.net_speed_in+1)*8 : 0, s.net_speed_out ? Math.log10(s.net_speed_out+1)*8 : 0]]);
}
function push(ch, [label, vals]) {
  ch.data.labels.push(label);
  if (ch.data.labels.length > 60) ch.data.labels.shift();
  ch.data.datasets.forEach((ds, i) => {
    ds.data.push(vals[i]);
    if (ds.data.length > 60) ds.data.shift();
  });
  ch.update("none");
}

// 改名:点击标题旁的铅笔,弹窗输入新名字
// 绑定详情页的改名按钮
function bindRename(id) {
  const btn = document.getElementById("renameBtn");
  if (!btn) return;
  btn.onclick = (e) => { e.preventDefault(); renameAgent(id); };
}
// 通用改名逻辑(卡片和详情页共用),改完自动刷新当前视图
async function renameAgent(id) {
  const cur = states[id] && states[id].name ? states[id].name : "";
  const name = prompt("修改主机名称:", cur);
  if (name === null) return; // 取消
  const trimmed = name.trim();
  if (!trimmed) { alert("名称不能为空"); return; }
  try {
    const r = await fetch(`/api/servers/${encodeURIComponent(id)}/rename`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: trimmed }),
    });
    if (r.ok) {
      if (states[id]) states[id].name = trimmed;
      if (selected === id) {
        const h1 = document.getElementById("titleName");
        if (h1) h1.textContent = trimmed;
      } else {
        renderList(); // 卡片列表刷新名字
      }
    } else {
      alert("改名失败: " + r.status);
    }
  } catch (e) { alert("网络错误"); }
}

async function loadHistory(id) {
  try {
    const tok = new URLSearchParams(location.search).get("token");
    const qs = tok ? `?token=${encodeURIComponent(tok)}&minutes=60` : "?minutes=60";
    const r = await fetch(`/api/servers/${encodeURIComponent(id)}/history${qs}`);
    const rows = await r.json();
    const c = charts[id];
    if (!c || !rows) return;
    for (const row of rows) {
      const t = fmt.time(row.ts);
      c.cpu.data.labels.push(t);
      c.cpu.data.datasets[0].data.push(row.cpu);
      const memUsed = (row.mem_used / (row.mem_total||1)) * 100;
      c.net.data.labels.push(t);
      c.net.data.datasets[0].data.push(memUsed);
    }
    c.cpu.update("none"); c.net.update("none");
  } catch (e) {}
}


function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, m => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[m]));
}

// 启动
const header = `<header><div class="brand"><span class="dot"></span>探针 · 服务器监控</div><div class="summary">连接中…</div></header>`;
document.body.insertAdjacentHTML("afterbegin", header);
connect();
route();
