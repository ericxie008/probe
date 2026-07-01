#!/usr/bin/env bash
# 探针 Agent 一键安装脚本(Linux / OpenWrt)。
set -euo pipefail

INSTALL_DIR="/opt/probe-agent"
GOVERSION="1.23.4"

G() { printf '\033[32m%s\033[0m\n' "$1"; }
R() { printf '\033[31m%s\033[0m\n' "$1"; }
Y() { printf '\033[33m%s\033[0m\n' "$1"; }

[[ $(id -u) -ne 0 ]] && { R "请用 root 运行"; exit 1; }

: "${SERVER:?需要指定 Dashboard 地址,如 SERVER=host:8553}"
: "${TOKEN:?需要指定 Agent 密钥,如 TOKEN=xxx}"
# OpenWrt 没有 hostname 命令,用 /proc/sys/kernel/hostname 或 cat 兜底
get_hostname() {
  hostname 2>/dev/null || \
  cat /proc/sys/kernel/hostname 2>/dev/null || \
  echo "openwrt"
}
NAME="${NAME:-$(get_hostname)}"
INTERVAL="${INTERVAL:-3s}"
USE_TLS="${TLS:-0}"
INSECURE_FLAG="${INSECURE:-0}"

# 检测是否为 OpenWrt(用 procd 而非 systemd)
IS_OPENWRT=0
if [[ -f /etc/openwrt_release ]] || ! command -v systemctl >/dev/null 2>&1; then
  IS_OPENWRT=1
fi

# --------------------------------------------------------------------
# Go: 从官方安装
# --------------------------------------------------------------------
ensure_go() {
  if command -v go >/dev/null 2>&1; then
    local gver
    gver=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | sed 's/go//')
    if [[ -n "$gver" ]]; then
      local major="${gver%%.*}" minor="${gver#*.}"
      local ver_num=$(( major * 100 + minor ))
      if (( ver_num >= 121 )); then
        G "Go $(go version | awk '{print $3}') 满足要求"
        return 0
      fi
    fi
    Y "系统 Go 版本太旧,需要 1.21+"
  else
    Y "未检测到 Go"
  fi

  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    armv7l|armv6l) arch="armv6l" ;;
    mips|mipsle|mipsel) arch="mipsle" ;;
    *) R "不支持的 CPU 架构: $arch"; exit 1 ;;
  esac

  G "==> 从官方下载 Go ${GOVERSION} linux/${arch} ..."
  cd /tmp
  curl -fsSL "https://go.dev/dl/go${GOVERSION}.linux-${arch}.tar.gz" -o go.tar.gz || {
    R "下载失败,请手动安装 Go 1.21+"; exit 1
  }
  rm -rf /usr/local/go
  tar -C /usr/local -xzf go.tar.gz
  rm -f go.tar.gz
  export PATH="/usr/local/go/bin:$PATH"
  grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null \
    || echo 'export PATH="/usr/local/go/bin:$PATH"' >> /etc/profile
  cd - >/dev/null
  G "Go 安装完成"
}

ensure_go

# --------------------------------------------------------------------
# 编译
# --------------------------------------------------------------------
G "==> 编译 Agent"
mkdir -p "$INSTALL_DIR"
export PATH="/usr/local/go/bin:$PATH"
if [[ -f "cmd/agent/main.go" ]]; then
  GOOS=linux go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/agent" ./cmd/agent \
    || { R "编译失败"; exit 1; }
else
  R "找不到源码 cmd/agent/main.go,请在项目目录下运行。"; exit 1
fi

EXTRA_ARGS=""
[[ "$USE_TLS" == "1" ]] && EXTRA_ARGS="$EXTRA_ARGS -tls"
[[ "$INSECURE_FLAG" == "1" ]] && EXTRA_ARGS="$EXTRA_ARGS -insecure"

# --------------------------------------------------------------------
# 服务配置
# --------------------------------------------------------------------
if [[ "$IS_OPENWRT" == "1" ]]; then
  G "==> 配置 OpenWrt init.d 服务"
  cat > /etc/init.d/probe-agent << 'INITEOF'
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    procd_open_instance
    procd_set_param command /opt/probe-agent/agent \
        -server "__SERVER__" \
        -token "__TOKEN__" \
        -name "__NAME__" \
        -interval "__INTERVAL__" \
        __EXTRA__
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}
INITEOF
  # 替换占位符
  sed -i "s|__SERVER__|$SERVER|g" /etc/init.d/probe-agent
  sed -i "s|__TOKEN__|$TOKEN|g" /etc/init.d/probe-agent
  sed -i "s|__NAME__|$NAME|g" /etc/init.d/probe-agent
  sed -i "s|__INTERVAL__|$INTERVAL|g" /etc/init.d/probe-agent
  sed -i "s|__EXTRA__|$EXTRA_ARGS|g" /etc/init.d/probe-agent
  chmod +x /etc/init.d/probe-agent
  /etc/init.d/probe-agent enable
  /etc/init.d/probe-agent restart
else
  G "==> 配置 systemd 服务"
  SERVICE_FILE="/etc/systemd/system/probe-agent.service"
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
fi

sleep 1
G ""
G "========================================="
G "  Agent 部署完成!"
G "========================================="
echo
echo "  主机名: $NAME"
echo "  连接到: $SERVER"
echo
if [[ "$IS_OPENWRT" == "1" ]]; then
  echo "  状态: /etc/init.d/probe-agent status"
  echo "  日志: logread -e probe-agent"
else
  echo "  状态: systemctl status probe-agent"
  echo "  日志: journalctl -u probe-agent -f"
fi
echo
G "服务已启动,即将在 Dashboard 上看到此主机。"
