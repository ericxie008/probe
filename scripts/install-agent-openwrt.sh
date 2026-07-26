#!/bin/sh
# 探针 Agent 一键安装(OpenWrt / 嵌入式设备)。
# 直接下载预编译静态二进制,不依赖 git / Go / sudo / bash。
# 用法:
#   SERVER=host:8553 TOKEN=xxx TLS=1 INSECURE=1 sh scripts/install-agent-openwrt.sh
# 或一行(curl 管道):
#   curl -fsSL <raw-url> | SERVER=... TOKEN=... TLS=1 INSECURE=1 sh
set -eu

REPO="ericxie008/probe"
VERSION="v0.1.0"
INSTALL_DIR="/opt/probe-agent"
ENV_FILE="$INSTALL_DIR/probe.env"
BIN_URL="https://github.com/${REPO}/releases/download/${VERSION}"

log()  { printf '\033[32m%s\033[0m\n' "$1"; }
warn() { printf '\033[33m%s\033[0m\n' "$1"; }
err()  { printf '\033[31m%s\033[0m\n' "$1"; }

# OpenWrt 默认就是 root,这里只是兜底校验
[ "$(id -u)" -eq 0 ] || { err "请用 root 运行"; exit 1; }

[ -n "${SERVER:-}" ] || { err "需要指定 Dashboard 地址,如 SERVER=host:8553"; exit 1; }
[ -n "${TOKEN:-}" ]  || { err "需要指定 Agent 密钥,如 TOKEN=xxx"; exit 1; }

# 主机名:优先 /proc,避免依赖 hostname 命令(部分精简固件没有)
NAME="${NAME:-$(cat /proc/sys/kernel/hostname 2>/dev/null || echo openwrt)}"
INTERVAL="${INTERVAL:-3s}"

# NAME 会被原样嵌入 init 脚本的命令行,必须限制字符集以防 shell 注入。
# 用黑名单:只要不含危险 shell 元字符即放行(中文/Unicode 安全)。
validate_name() {
  printf '%s' "$1" | grep -Eq '["'"'"'`$;|&<>\]'
}
if validate_name "$NAME"; then
  err "NAME 含非法字符(禁止 \" ' \` \$ ; | & < > \\\\)"; exit 1
fi

# 架构映射到预编译产物名
case "$(uname -m)" in
  x86_64|amd64)    ARCH="amd64" ;;
  aarch64|arm64)   ARCH="arm64" ;;
  armv7l|armv6l|armv7) ARCH="arm-7" ;;
  mips|mipsle|mipsel)  ARCH="mipsle" ;;
  *) err "不支持的 CPU 架构: $(uname -m)"; exit 1 ;;
esac

# TLS / 跳过校验
EXTRA="-server \"$SERVER\" -name \"$NAME\" -interval \"$INTERVAL\""
[ "${TLS:-0}" = "1" ]      && EXTRA="$EXTRA -tls"
[ "${INSECURE:-0}" = "1" ] && EXTRA="$EXTRA -insecure"

mkdir -p "$INSTALL_DIR"

log "==> 下载 agent (linux/$ARCH $VERSION)"
URL="$BIN_URL/agent-linux-$ARCH"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$INSTALL_DIR/agent.new"
else
  wget -qO "$INSTALL_DIR/agent.new" "$URL"
fi
# 兜底校验:GitHub 404 时 curl -f 已会失败;这里再防异常 HTML
if [ "$(wc -c < "$INSTALL_DIR/agent.new")" -lt 100000 ]; then
  err "下载的文件太小,可能该架构未发布或 VERSION 不对: $URL"
  cat "$INSTALL_DIR/agent.new" 1>&2 || true
  rm -f "$INSTALL_DIR/agent.new"
  exit 1
fi
mv "$INSTALL_DIR/agent.new" "$INSTALL_DIR/agent"
chmod +x "$INSTALL_DIR/agent"

# 凭据写入独立 600 文件,不写进 init 脚本(后者默认全机可读)
log "==> 写入配置"
cat > "$ENV_FILE" <<EOF
PROBE_TOKEN=$TOKEN
EOF
chmod 600 "$ENV_FILE"

# procd 服务:命令参数直接烤进去,只有 PROBE_TOKEN 运行时从 env 文件加载
log "==> 配置 OpenWrt init.d 服务"
cat > /etc/init.d/probe-agent <<EOF
#!/bin/sh /etc/rc.common
START=99
USE_PROCD=1
start_service() {
    . $ENV_FILE
    procd_open_instance
    procd_set_param command $INSTALL_DIR/agent $EXTRA
    procd_set_param env PROBE_TOKEN="\$PROBE_TOKEN"
    procd_set_param respawn
    procd_set_param stdout 1
    procd_set_param stderr 1
    procd_close_instance
}
EOF
chmod +x /etc/init.d/probe-agent
/etc/init.d/probe-agent enable
/etc/init.d/probe-agent restart

sleep 1
log ""
log "========================================="
log "  Agent 部署完成!"
log "========================================="
echo
echo "  主机名: $NAME"
echo "  连接到: $SERVER"
echo "  架构:   linux/$ARCH ($VERSION)"
echo
echo "  状态: /etc/init.d/probe-agent status"
echo "  日志: logread -e probe-agent"
echo
log "服务已启动,即将在 Dashboard 上看到此主机。"
