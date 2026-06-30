#!/usr/bin/env bash
# 探针 Dashboard 一键安装脚本(Linux)。
#
# 选项(环境变量):
#   DOMAIN=监控.example.com   用域名签发 Let's Encrypt
#   CERT=/path/fullchain.pem  使用已有证书(优先级最高)
#   KEY=/path/privkey.pem     与 CERT 配对
#   SECRET=<agent密钥>        Agent 认证密钥(留空自动生成)
#   WEB_TOKEN=<访问口令>      Web 登录口令(留空自动生成)
#   PORT=8553                 监听端口
set -euo pipefail

INSTALL_DIR="/opt/probe"
SERVICE_FILE="/etc/systemd/system/probe-dashboard.service"
DATA_DIR="$INSTALL_DIR/data"
GOVERSION="1.23.4"

G() { printf '\033[32m%s\033[0m\n' "$1"; }
Y() { printf '\033[33m%s\033[0m\n' "$1"; }
R() { printf '\033[31m%s\033[0m\n' "$1"; }

[[ $EUID -ne 0 ]] && { R "请用 root 或 sudo 运行"; exit 1; }

# --------------------------------------------------------------------
# Go: 始终从官方安装新版,不依赖系统包管理器(Debian/Ubuntu 的版本太旧)
# --------------------------------------------------------------------
ensure_go() {
  local need=21
  # 已经有 go?
  if command -v go >/dev/null 2>&1; then
    local have
    have=$(go version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d 'go')
    if [[ -n "$have" ]]; then
      local major="${have%%.*}" minor="${have#*.}"
      if (( major > need )) || (( major == need )); then
        G "Go $(go version | awk '{print $3}') 满足要求"
        return 0
      fi
    fi
    Y "系统 Go 版本太旧($(go version | awk '{print $3}')),需要 1.${need}+"
  else
    Y "未检测到 Go"
  fi

  # 确定架构
  local arch
  arch=$(uname -m)
  case "$arch" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) R "不支持的 CPU 架构: $arch"; exit 1 ;;
  esac

  G "==> 从官方下载 Go ${GOVERSION} linux/${arch} ..."
  cd /tmp
  curl -fsSL "https://go.dev/dl/go${GOVERSION}.linux-${arch}.tar.gz" -o go.tar.gz || {
    R "下载失败,请手动安装 Go 1.${need}+: https://go.dev/dl/"; exit 1
  }
  rm -rf /usr/local/go
  tar -C /usr/local -xzf go.tar.gz
  rm -f go.tar.gz
  export PATH="/usr/local/go/bin:$PATH"
  grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null \
    || echo 'export PATH="/usr/local/go/bin:$PATH"' >> /etc/profile
  cd - >/dev/null
  G "Go $(go version | awk '{print $3}') 安装完成"
}

ensure_go

DOMAIN="${DOMAIN:-}"
SECRET="${SECRET:-$(openssl rand -hex 16)}"
WEB_TOKEN="${WEB_TOKEN:-$(openssl rand -hex 8)}"
PORT="${PORT:-8553}"

G "==> 准备目录 $INSTALL_DIR"
mkdir -p "$INSTALL_DIR" "$DATA_DIR"

# 编译
if [[ -f "cmd/dashboard/main.go" ]]; then
  G "==> 编译 Dashboard"
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/dashboard" ./cmd/dashboard \
    || { R "编译失败,请检查上方报错"; exit 1; }
  cp -r web "$INSTALL_DIR/"
else
  R "找不到源码 cmd/dashboard/main.go,请在项目目录下运行。"; exit 1
fi

# --------------------------------------------------------------------
# TLS 证书: 显式传入 > 域名申请 > 自签
# --------------------------------------------------------------------
CERT_PATH="${CERT:-}"
KEY_PATH="${KEY:-}"

if [[ -n "$CERT_PATH" && -n "$KEY_PATH" ]]; then
  G "==> 使用已有证书: $CERT_PATH"
  [[ -f "$CERT_PATH" ]] || { R "证书文件不存在: $CERT_PATH"; exit 1; }
  [[ -f "$KEY_PATH" ]] || { R "私钥文件不存在: $KEY_PATH"; exit 1; }
  TLS_ARGS="-tls-cert $CERT_PATH -tls-key $KEY_PATH"
elif [[ -n "$DOMAIN" ]]; then
  G "==> 尝试用 Let's Encrypt 为 $DOMAIN 签发证书"
  command -v certbot >/dev/null 2>&1 || {
    Y "安装 certbot..."
    if command -v apt-get >/dev/null 2>&1; then apt-get install -y -qq certbot
    elif command -v yum >/dev/null 2>&1; then yum install -y -q certbot
    fi
  }
  if certbot certonly --standalone -d "$DOMAIN" --non-interactive --agree-tos -m "admin@${DOMAIN}" --keep-until-expiring 2>/dev/null; then
    TLS_ARGS="-tls-cert /etc/letsencrypt/live/$DOMAIN/fullchain.pem -tls-key /etc/letsencrypt/live/$DOMAIN/privkey.pem"
    G "Let's Encrypt 证书签发成功"
  else
    Y "Let's Encrypt 签发失败,改用自签证书"
    openssl req -x509 -newkey rsa:2048 -keyout "$DATA_DIR/key.pem" -out "$DATA_DIR/cert.pem" -days 3650 -nodes -subj "/CN=$DOMAIN" 2>/dev/null
    TLS_ARGS="-tls-cert $DATA_DIR/cert.pem -tls-key $DATA_DIR/key.pem"
  fi
else
  G "==> 生成自签证书(公网建议用 DOMAIN=域名 或 CERT=已有证书)"
  openssl req -x509 -newkey rsa:2048 -keyout "$DATA_DIR/key.pem" -out "$DATA_DIR/cert.pem" -days 3650 -nodes -subj "/CN=localhost" 2>/dev/null
  TLS_ARGS="-tls-cert $DATA_DIR/cert.pem -tls-key $DATA_DIR/key.pem"
fi

# --------------------------------------------------------------------
# systemd
# --------------------------------------------------------------------
G "==> 配置 systemd 服务"
cat > "$SERVICE_FILE" <<UNITSVC
[Unit]
Description=Probe Server Monitor Dashboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/dashboard \\
  -addr :$PORT \\
  -secret "$SECRET" \\
  -web-token "$WEB_TOKEN" \\
  -db $DATA_DIR/probe.db \\
  $TLS_ARGS
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNITSVC

systemctl daemon-reload
systemctl enable --now probe-dashboard
sleep 1

G ""
G "================================================"
G "  Dashboard 部署完成!"
G "================================================"
echo
IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[[ -z "$IP" ]] && IP="服务器IP"
echo "  访问地址:      https://$IP:$PORT/?token=$WEB_TOKEN"
echo "  Agent 密钥:    $SECRET"
echo "  Web 访问口令:  $WEB_TOKEN"
echo
Y "  Agent 连接命令(在被监控机器上执行):"
echo "    ./agent -server ${DOMAIN:-$IP}:$PORT -token $SECRET -name \"主机名\" -tls -insecure"
echo
echo "  服务管理:"
echo "    systemctl status probe-dashboard"
echo "    journalctl -u probe-dashboard -f"
echo
Y "  请妥善保存上面的密钥,这不会再次显示。"
