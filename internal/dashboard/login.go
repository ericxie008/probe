package dashboard

// loginHTML is the password-entry page served at /login.
const loginHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>探针 · 登录</title>
<style>
:root {
  --bg: #0b1220; --bg-grad: radial-gradient(ellipse 90% 50% at 50% -10%, #16213a 0%, transparent 70%);
  --panel: #111827; --border: #243044;
  --text: #e2e8f0; --muted: #7c8aa5; --accent: #3b82f6; --red: #f87171;
  --green: #34d399;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body {
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
  background: var(--bg); 
  color: var(--text); min-height: 100vh;
  display: flex; align-items: center; justify-content: center;
}
.card {
  background: var(--panel); border: 1px solid var(--border); border-radius: 10px;
  padding: 36px 32px; width: 340px; max-width: 90vw;
}
.brand { display: flex; align-items: center; gap: 10px; margin-bottom: 4px; font-size: 18px; font-weight: 600; }
.brand .dot { width: 10px; height: 10px; border-radius: 50%; background: #16a34a; box-shadow: 0 0 8px #16a34a; }
.sub { color: var(--muted); font-size: 13px; margin-bottom: 24px; }
label { display: block; font-size: 13px; color: var(--muted); margin-bottom: 6px; }
input {
  width: 100%; background: var(--bg); border: 1px solid var(--border);
  border-radius: 7px; padding: 10px 12px; color: var(--text); font-size: 14px;
  outline: none; transition: border-color .15s;
}
input:focus { border-color: var(--accent); }
.btn {
  width: 100%; margin-top: 18px; background: var(--accent); color: #fff;
  border: none; border-radius: 7px; padding: 10px; font-size: 14px; font-weight: 600;
  cursor: pointer; transition: opacity .15s;
}
.btn:hover { opacity: .9; }
.btn:disabled { opacity: .5; cursor: wait; }
.err { color: var(--red); font-size: 13px; margin-top: 12px; min-height: 18px; }
@media (max-width: 480px) {
  .card { padding: 28px 20px; width: 100%; border-radius: 0; border: none; }
}
</style>
</head>
<body>
<div class="card">
  <div class="brand"><span class="dot"></span>探针 · 服务器监控</div>
  <div class="sub">请输入访问口令</div>
  <form id="f">
    <label for="pw">访问口令</label>
    <input id="pw" type="password" autofocus autocomplete="current-password" />
    <button class="btn" type="submit" id="b">登录</button>
    <div class="err" id="e"></div>
  </form>
</div>
<script>
const f = document.getElementById("f"), pw = document.getElementById("pw"),
      b = document.getElementById("b"), e = document.getElementById("e");
f.onsubmit = async (ev) => {
  ev.preventDefault();
  b.disabled = true; e.textContent = "";
  try {
    const r = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: pw.value }),
    });
    if (r.ok) { location.href = "/"; }
    else { e.textContent = r.status === 401 ? "口令错误" : "登录失败"; b.disabled = false; pw.select(); }
  } catch (err) { e.textContent = "网络错误"; b.disabled = false; }
};
</script>
</body>
</html>`
