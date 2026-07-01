// --- 探针前端: 服务器列表 / 详情 / 实时图表 ---
const app = document.getElementById("app");
let states = {};        // agentID -> latest state
let selected = null;    // current agentID in detail view
let ws = null;
const charts = {};      // agentID -> {cpu, mem, net}
let overrideNames = {}; // 本地缓存改名,防止 WS 推送覆盖
let sortKey = "status"; // 排序字段: status|name|cpu|mem|disk|net
let sortDir = -1;       // -1=降序(高优先), 1=升序
let renderTimer = null;  // 渲染节流

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
    return new Date(ts * 1000).toLocaleTimeString("zh-CN");
  },
};

function connect() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
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
      if (overrideNames[msg.data.agent_id]) msg.data.name = overrideNames[msg.data.agent_id];
      states[msg.data.agent_id] = msg.data;
      if (selected === msg.data.agent_id) updateDetail();
      else if (!selected) {
        // 节流:100ms 内多次更新只渲染一次,减少 DOM 操作
        if (renderTimer) clearTimeout(renderTimer);
        renderTimer = setTimeout(() => { renderTimer = null; renderList(); }, 100);
      }
    }
  };
}

// ---------- 排序 ----------
function sortedIds() {
  const ids = Object.keys(states);
  const val = (s) => {
    switch (sortKey) {
      case "name":   return (s.name || "").toLowerCase();
      case "cpu":    return s.cpu_usage || 0;
      case "mem":    return s.memory_total ? s.memory_used / s.memory_total : 0;
      case "disk":   return s.disk_total ? s.disk_used / s.disk_total : 0;
      case "net":    return (s.net_speed_in || 0) + (s.net_speed_out || 0);
      case "status": return isOnline(s) ? 1 : 0;
      default:       return 0;
    }
  };
  ids.sort((a, b) => {
    const va = val(states[a]), vb = val(states[b]);
    if (typeof va === "string") return sortDir * va.localeCompare(vb);
    return sortDir * (va > vb ? 1 : va < vb ? -1 : 0);
  });
  return ids;
}

function setSort(key) {
  if (sortKey === key) { sortDir *= -1; }
  else { sortKey = key; sortDir = (key === "name") ? 1 : -1; }
  renderList();
}

// ---------- 路由 ----------
function route() {
  const id = location.hash.replace("#/", "");
  if (id && states[id]) { selected = id; renderDetail(); }
  else { selected = null; renderList(); }
}
window.addEventListener("hashchange", route);

// ---------- 列表 ----------
function renderList() {
  const ids = sortedIds();
  document.querySelector("header .summary").textContent =
    `${ids.length} 台服务器 · ${ids.filter(id => isOnline(states[id])).length} 在线`;
  if (ids.length === 0) {
    app.innerHTML = `<div class="empty"><span class="spin"></span><p style="margin-top:12px">等待 Agent 连接…</p>
      <p style="color:var(--muted);font-size:12px;margin-top:8px">在服务器上运行 agent 即可接入监控</p></div>`;
    return;
  }
  let html = '<main><div class="toolbar">';
  const opts = [["status","状态"],["name","名称"],["cpu","CPU"],["mem","内存"],["disk","磁盘"],["net","流量"]];
  for (const [k, label] of opts) {
    const active = sortKey === k;
    const arrow = active ? (sortDir === 1 ? " ↑" : " ↓") : "";
    html += `<button class="sort-btn${active ? " active" : ""}" onclick="setSort('${k}')">${label}${arrow}</button>`;
  }
  html += '</div><div class="grid">';
  for (const id of ids) html += card(states[id]);
  html += "</div></main>";
  app.innerHTML = html;
  for (const el of app.querySelectorAll(".card")) {
    el.onclick = () => { location.hash = "/" + el.dataset.id; };
  }
  app.querySelectorAll(".edit-btn-sm").forEach(btn => {
    btn.onclick = (e) => { e.stopPropagation(); renameAgent(btn.dataset.rename); };
  });
  app.querySelectorAll(".del-btn").forEach(btn => {
    btn.onclick = (e) => { e.stopPropagation(); deleteAgent(btn.dataset.del); };
  });
}

function card(s) {
  const online = isOnline(s);
  const memP = fmt.pct(s.memory_used, s.memory_total);
  const cpuP = fmt.pct(s.cpu_usage, 100);
  const diskP = fmt.pct(s.disk_used, s.disk_total);
  return `<div class="card${online ? "" : " offline-card"}" data-id="${s.agent_id}">
    <div class="head">
      <div>
        <div class="name-row"><span class="name">${esc(s.name)}</span><button class="edit-btn-sm" data-rename="${s.agent_id}" title="修改名称">✎</button>${online ? "" : `<button class="edit-btn-sm del-btn" data-del="${s.agent_id}" title="删除此记录">✕</button>`}</div>
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
  return Date.now() / 1000 - s.timestamp < 30;
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
  loadHistory(selected);
}

function updateDetail() {
  const s = states[selected];
  if (!s || !document.getElementById("diskTable")) return;
  fillTable("diskTable", (s.disks||[]).map(d => [
    d.mountpoint, d.device, d.fs_type,
    fmt.bytes(d.used), fmt.bytes(d.total), fmt.pct(d.used, d.total),
  ]));
  fillTable("netTable", (s.interfaces||[]).map(i => [i.name, i.ipv4||"—", i.ipv6||"—", i.mac||"—"]));
  fillTable("procTable", (s.processes||[]).map(p => [p.pid, p.name, (p.cpu||0).toFixed(1)+"%", fmt.bytes(p.memory)]));
  pushChart(s);
}

function fillTable(id, rows) {
  const tb = document.querySelector("#" + id + " tbody");
  if (!tb) return;
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
  // 销毁旧实例,防止内存泄漏
  if (charts[selected]) {
    if (charts[selected].cpu) charts[selected].cpu.destroy();
    if (charts[selected].net) charts[selected].net.destroy();
  }
  charts[selected] = {
    cpu: makeChart("cpuChart", [{c:"#2f81f7",label:"CPU"}], 100),
    net: makeChart("netChart", [
      {c:"#a371f7",label:"内存%"},
      {c:"#3fb950",label:"↓"},
      {c:"#d29922",label:"↑"},
    ]),
  };
}
function makeChart(id, colors, max) {
  const ctx = document.getElementById(id);
  if (!ctx) return null;
  return new Chart(ctx, {
    type: "line",
    data: { labels: [], datasets: colors.map(d => ({ label: d.label, borderColor: d.c, backgroundColor: d.c+"22", data: [], tension: .3, pointRadius: 0, borderWidth: 2, fill: true })) },
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

// ---------- 改名 ----------
function bindRename(id) {
  const btn = document.getElementById("renameBtn");
  if (!btn) return;
  btn.onclick = (e) => { e.preventDefault(); renameAgent(id); };
}
async function renameAgent(id) {
  const cur = states[id] && states[id].name ? states[id].name : "";
  const name = prompt("修改主机名称:", cur);
  if (name === null) return;
  const trimmed = name.trim();
  if (!trimmed) { alert("名称不能为空"); return; }
  try {
    const r = await fetch(`/api/servers/${encodeURIComponent(id)}/rename`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: trimmed }),
    });
    if (r.ok) {
      overrideNames[id] = trimmed;
      if (states[id]) states[id].name = trimmed;
      if (selected === id) {
        const h1 = document.getElementById("titleName");
        if (h1) h1.textContent = trimmed;
      } else { renderList(); }
    } else { alert("改名失败: " + r.status); }
  } catch (e) { alert("网络错误"); }
}

async function deleteAgent(id) {
  const s = states[id];
  if (!s) return;
  if (!confirm(`确定删除 "${s.name || id}" ?\n此操作会清除该主机的历史数据,不可恢复。`)) return;
  try {
    const r = await fetch(`/api/servers/${encodeURIComponent(id)}/delete`, { method: "POST" });
    if (r.ok) {
      delete states[id];
      renderList();
    } else { alert("删除失败: " + r.status); }
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

// ---------- 部署面板 ----------
let deploySecret = "";
let deployCmds = {};
async function showDeploy() {
  // 密钥从管理员本地输入获取(不通过 API 传输,防泄露)
  if (!deploySecret) {
    deploySecret = localStorage.getItem("deploy_secret") || "";
  }
  // 如果没存过,从 API 拿打码版本用于显示提示
  let maskedSecret = "";
  if (!deploySecret) {
    try {
      const r = await fetch("/api/deploy");
      const d = await r.json();
      maskedSecret = d.secret_masked || "";
    } catch (e) {}
  }
  const host = location.host;
  const hostname = host.split(":")[0];
  const isTLS = location.protocol === "https:";
  const tlsFlag = isTLS ? " -tls" : "";
  const insecureFlag = isTLS ? " -insecure" : "";

  const cloneUrl = "https://github.com/ericxie008/probe.git";

  const installCmd = `# 一键安装 Agent(Linux)\n` +
    `git clone ${cloneUrl} probe && cd probe\n` +
    `sudo SERVER=${host} TOKEN=${deploySecret || "<填入密钥: " + maskedSecret + ">"} TLS=1${isTLS ? " INSECURE=1" : ""} ./scripts/install-agent.sh\n` +
    `# NAME 不传默认用系统主机名,自动唯一`;

  const upgradeCmd = `# 升级 Agent\n` +
    `cd ~/probe && git pull\n` +
    `export PATH="/usr/local/go/bin:$PATH"\n` +
    `go build -trimpath -ldflags "-s -w" -o /opt/probe-agent/agent ./cmd/agent\n` +
    `# 确保 service 文件存在\n` +
    `systemctl cat probe-agent >/dev/null 2>&1 || sudo SERVER=${host} TOKEN=${deploySecret || "<密钥>"} TLS=1${isTLS ? " INSECURE=1" : ""} ./scripts/install-agent.sh\n` +
    `systemctl restart probe-agent`;

  const manualCmd = `# 手动运行 Agent(不用脚本)\n` +
    `# -name 可选,不传默认用主机名\n` +
    `./agent -server ${host} -token ${deploySecret || "<填入密钥>"}${tlsFlag}${insecureFlag}`;

  const dashInstallCmd = `# 一键安装 Dashboard(服务端)\n` +
    `git clone ${cloneUrl} probe && cd probe\n` +
    `sudo CERT="你的证书路径" KEY="你的私钥路径" ./scripts/install-dashboard.sh\n` +
    `# 无已有证书用域名自动申请:\n` +
    `# sudo DOMAIN=${hostname} ./scripts/install-dashboard.sh`;

  const dashUpgradeCmd = `# 升级 Dashboard\n` +
    `cd ~/probe && git pull\n` +
    `export PATH="/usr/local/go/bin:$PATH"\n` +
    `go build -trimpath -ldflags "-s -w" -o /opt/probe/dashboard ./cmd/dashboard\n` +
    `cp -r web /opt/probe/\n` +
    `mkdir -p /opt/probe/data\n` +
    `# 确保 service 文件存在(不存在则自动创建)\n` +
    `systemctl cat probe-dashboard >/dev/null 2>&1 || sudo ./scripts/install-dashboard.sh\n` +
    `systemctl restart probe-dashboard`;

  deployCmds = { dashInstall: dashInstallCmd, dashUpgrade: dashUpgradeCmd, install: installCmd, upgrade: upgradeCmd, manual: manualCmd };

  // 构建模态框
  const modal = document.createElement("div");
  modal.className = "modal-overlay";
  modal.id = "deployModal";
  modal.onclick = (e) => { if (e.target === modal) modal.remove(); };
  modal.innerHTML = `
    <div class="modal">
      <div class="modal-head">
        <h2>部署与升级</h2>
        <button class="modal-close" onclick="document.getElementById('deployModal').remove()">✕</button>
      </div>
      <div class="modal-body">
        <div class="cmd-section token-input-row">
          <span class="token-label">Agent 密钥</span>
          <input type="text" id="secretInput" class="token-input" placeholder="${maskedSecret ? "已设置(" + maskedSecret + "),重新输入可覆盖" : "输入 Agent 密钥"}" value="${deploySecret}">
          <button class="copy-btn" onclick="saveSecret()">保存</button>
        </div>
        <div class="cmd-group-title">Dashboard(服务端)</div>
        <div class="cmd-section">
          <div class="cmd-title"><span>安装 Dashboard</span><button class="copy-btn" onclick="copyCmd(this, 'dashInstall')">复制</button></div>
          <pre class="cmd-block" id="cmdDashInstall"></pre>
        </div>
        <div class="cmd-section">
          <div class="cmd-title"><span>升级 Dashboard</span><button class="copy-btn" onclick="copyCmd(this, 'dashUpgrade')">复制</button></div>
          <pre class="cmd-block" id="cmdDashUpgrade"></pre>
        </div>
        <div class="cmd-group-title">Agent(被监控机)</div>
        <div class="cmd-section">
          <div class="cmd-title"><span>安装新 Agent</span><button class="copy-btn" onclick="copyCmd(this, 'install')">复制</button></div>
          <pre class="cmd-block" id="cmdInstall"></pre>
        </div>
        <div class="cmd-section">
          <div class="cmd-title"><span>升级已有 Agent</span><button class="copy-btn" onclick="copyCmd(this, 'upgrade')">复制</button></div>
          <pre class="cmd-block" id="cmdUpgrade"></pre>
        </div>
        <div class="cmd-section">
          <div class="cmd-title"><span>手动运行</span><button class="copy-btn" onclick="copyCmd(this, 'manual')">复制</button></div>
          <pre class="cmd-block" id="cmdManual"></pre>
        </div>
      </div>
    </div>`;
  document.body.appendChild(modal);
  // 填入文本(避免 HTML 注入,用 textContent)
  document.getElementById("cmdDashInstall").textContent = dashInstallCmd;
  document.getElementById("cmdDashUpgrade").textContent = dashUpgradeCmd;
  document.getElementById("cmdInstall").textContent = installCmd;
  document.getElementById("cmdUpgrade").textContent = upgradeCmd;
  document.getElementById("cmdManual").textContent = manualCmd;
}

function saveSecret() {
  const v = document.getElementById("secretInput").value.trim();
  deploySecret = v;
  localStorage.setItem("deploy_secret", v);
  document.getElementById("deployModal").remove();
  showDeploy();
}

function copyCmd(btn, key) {
  const text = deployCmds[key] || "";
  navigator.clipboard.writeText(text).then(() => {
    btn.textContent = "已复制";
    setTimeout(() => btn.textContent = "复制", 1500);
  }).catch(() => {
    // fallback
    const ta = document.createElement("textarea");
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand("copy"); ta.remove();
    btn.textContent = "已复制"; setTimeout(() => btn.textContent = "复制", 1500);
  });
}

async function doLogout() {
  try {
    await fetch("/api/logout", { method: "POST" });
  } catch (e) {}
  location.href = "/login";
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, m => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[m]));
}

// 启动
const header = `<header><div class="brand"><span class="dot"></span>探针 · 服务器监控</div><div class="header-right"><span class="summary">连接中…</span><button class="deploy-btn" onclick="showDeploy()" title="部署与升级">部署</button><button class="logout-btn" id="logoutBtn" title="退出登录" onclick="doLogout()">退出</button></div></header>`;
document.body.insertAdjacentHTML("afterbegin", header);
// 先初始化 overrideNames,再连接 WebSocket 和渲染,防止刷新后改名丢失
initOverrideNames().then(() => { connect(); route(); });

// 页面加载时从 API 拉取名字缓存,防止刷新后改名丢失
async function initOverrideNames() {
  try {
    const r = await fetch("/api/servers");
    const list = await r.json();
    if (Array.isArray(list)) {
      for (const s of list) {
        if (s.name) overrideNames[s.id] = s.name;
      }
    }
  } catch (e) {}
}
