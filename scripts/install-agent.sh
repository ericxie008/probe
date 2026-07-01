#!/usr/bin/env bash
# 探针 Agent 一键安装脚本(Linux)。
#
# 用法:
#   sudo SERVER=your.domain:8553 TOKEN=xxx NAME=主机名 ./scripts/install-agent.sh
#   TLS=1 启用加密; 自签证书再加 INSECURE=1
set -euo pipefail

INSTALL_DIR="/opt/probe-agent"
SERVICE_FILE="/etc/systemd/system/probe-agent.service"
GOVERSION="1.23.4"

G() { printf '\033[32m%s\033[0m\n' "$1"; }
R() { printf '\033[31m%s\033[0m\n' "$1"; }
Y() { printf '\033[33m%s\033[0m\n' "$1"; }

[[ $EUID -ne 0 ]] && { R "请用 root 或 sudo 运行"; exit 1; }

: "${SERVER:?需要指定 Dashboard 地址,如 SERVER=host:8553}"
: "${TOKEN:?需要指定 Agent 密钥,如 TOKEN=xxx}"
NAME="${NAME:-$(hostname)}"
INTERVAL="${INTERVAL:-3s}"
USE_TLS="${TLS:-0}"
INSECURE_FLAG="${INSECURE:-0}"

# --------------------------------------------------------------------
# Go: 从官方安装,不依赖系统包管理器
# --------------------------------------------------------------------
ensure_go() {
  if command -v go >/dev/null 2>&1; then
    local gver
    gver=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | sed 's/go//')
    if [[ -n "$gver" ]]; then
      local major="${gver%%.*}" minor="${gver#*.}"
      local ver_num=$(( major * 100 + minor ))
      if (( ver_num >= 121 )); then
        return 0
      fi
    fi
    Y "系统 Go 版本太旧,需要 1.21+"
  fi
  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) R "不支持的架构: $arch"; exit 1 ;;
  esac
  G "==> 从官方下载 Go ${GOVERSION} linux/${arch} ..."
  cd /tmp
  curl -fsSL "https://go.dev/dl/go${GOVERSION}.linux-${arch}.tar.gz" -o go.tar.gz || {
    R "下载失败,请手动安装 Go 1.${need}+"; exit 1
  }
  rm -rf /usr/local/go
  tar -C /usr/local -xzf go.tar.gz
  rm -f go.tar.gz
  export PATH="/usr/local/go/bin:$PATH"
  grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null \
    || echo 'export PATH="/usr/local/go/bin:$PATH"' >> /etc/profile
  cd - >/dev/null
}

ensure_go

G "==> 编译 Agent"
mkdir -p "$INSTALL_DIR"
if [[ -f "cmd/agent/main.go" ]]; then
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/agent" ./cmd/agent \
    || { R "编译失败"; exit 1; }
else
  R "找不到源码 cmd/agent/main.go,请在项目目录下运行。"; exit 1
fi

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
if systemctl is-active --quiet probe-agent 2>/dev/null; then
  G "服务已启动,即将在 Dashboard 上看到此主机。"
else
  Y "查看日志: journalctl -u probe-agent"
fi
