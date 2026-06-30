#!/usr/bin/env bash
# 探针 Agent 一键安装脚本(Linux)。
# 用法:
#   sudo SERVER=your.domain:8443 TOKEN=xxx NAME=主机名 ./scripts/install-agent.sh
#   或:
#   sudo SERVER=... TOKEN=... NAME=... TLS=1 INSECURE=1 ./scripts/install-agent.sh
set -euo pipefail

INSTALL_DIR="/opt/probe-agent"
SERVICE_FILE="/etc/systemd/system/probe-agent.service"

G() { printf '\033[32m%s\033[0m\n' "$1"; }
R() { printf '\033[31m%s\033[0m\n' "$1"; }
Y() { printf '\033[33m%s\033[0m\n' "$1"; }

[[ $EUID -ne 0 ]] && { R "请用 root 或 sudo 运行"; exit 1; }

: "${SERVER:?需要指定 Dashboard 地址,如 SERVER=host:8443}"
: "${TOKEN:?需要指定 Agent 密钥,如 TOKEN=xxx}"
NAME="${NAME:-$(hostname)}"
INTERVAL="${INTERVAL:-3s}"
USE_TLS="${TLS:-0}"
INSECURE_FLAG="${INSECURE:-0}"

# 安装 Go(如果缺失)
command -v go >/dev/null 2>&1 || {
  Y "未检测到 Go,正在安装..."
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq golang >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q golang >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q golang >/dev/null
  else
    R "请先安装 Go"; exit 1
  fi
}

G "==> 编译 Agent"
mkdir -p "$INSTALL_DIR"
if [[ -f "cmd/agent/main.go" ]]; then
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/agent" ./cmd/agent
elif [[ -d ".git" ]]; then
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/agent" ./cmd/agent
else
  R "找不到源码,请在此项目目录下运行。"; exit 1
fi

# 组装启动参数
EXTRA_ARGS=""
[[ "$USE_TLS" == "1" ]] && EXTRA_ARGS="$EXTRA_ARGS -tls"
[[ "$INSECURE_FLAG" == "1" ]] && EXTRA_ARGS="$EXTRA_ARGS -insecure"

G "==> 配置 systemd 服务"
cat > "$SERVICE_FILE" <<UNITSVC
[Unit]
Description=Probe Server Monitor Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/agent \\
  -server "$SERVER" \\
  -token "$TOKEN" \\
  -name "$NAME" \\
  -interval "$INTERVAL" \\
  $EXTRA_ARGS
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
UNITSVC

systemctl daemon-reload
systemctl enable --now probe-agent
sleep 1

G ""
G "========================================="
G "  Agent 部署完成!"
G "========================================="
echo
echo "  主机名: $NAME"
echo "  连接到: $SERVER"
echo
echo "  状态: systemctl status probe-agent"
echo "  日志: journalctl -u probe-agent -f"
echo
if systemctl is-active --quiet probe-agent; then
  G "服务已启动,即将在 Dashboard 上看到此主机。"
else
  Y "服务启动中,请查看日志: journalctl -u probe-agent"
fi
