#!/usr/bin/env bash
# 探针 Dashboard 一键安装脚本(Linux)。
# 用法:
#   curl -fsSL <你的地址>/install-dashboard.sh | sudo bash
#   或本地: sudo ./scripts/install-dashboard.sh
#
# 选项(环境变量):
#   DOMAIN=监控.example.com   用域名签发 Let's Encrypt(需要已解析到本机)
#   SECRET=<agent密钥>         Agent 认证密钥(留空则自动生成)
#   WEB_TOKEN=<访问口令>       Web UI 访问口令(留空则自动生成)
#   PORT=8443                  监听端口
#   CERT=/path/fullchain.pem   使用已有证书(优先级最高)
#   KEY=/path/privkey.pem      与 CERT 配对,两者必须同时提供
set -euo pipefail

INSTALL_DIR="/opt/probe"
SERVICE_FILE="/etc/systemd/system/probe-dashboard.service"
DATA_DIR="$INSTALL_DIR/data"

# 颜色
G() { printf '\033[32m%s\033[0m\n' "$1"; }
Y() { printf '\033[33m%s\033[0m\n' "$1"; }
R() { printf '\033[31m%s\033[0m\n' "$1"; }

[[ $EUID -ne 0 ]] && { R "请用 root 或 sudo 运行"; exit 1; }

command -v go >/dev/null 2>&1 || {
  Y "未检测到 Go,正在安装..."
  # 优先尝试包管理器
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -qq && apt-get install -y -qq golang >/dev/null
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q golang >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q golang >/dev/null
  else
    R "请先安装 Go (https://go.dev/dl/)"; exit 1
  fi
}

DOMAIN="${DOMAIN:-}"
SECRET="${SECRET:-$(openssl rand -hex 16)}"
WEB_TOKEN="${WEB_TOKEN:-$(openssl rand -hex 8)}"
PORT="${PORT:-8443}"

G "==> 准备目录 $INSTALL_DIR"
mkdir -p "$INSTALL_DIR" "$DATA_DIR"

# 如果有源码就用源码编译,没有就尝试拉取
if [[ -f "cmd/dashboard/main.go" ]]; then
  G "==> 从源码编译 Dashboard"
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/dashboard" ./cmd/dashboard 2>/dev/null
  cp -r web "$INSTALL_DIR/"
elif [[ -d ".git" ]]; then
  G "==> 从 Git 编译"
  go build -trimpath -ldflags "-s -w" -o "$INSTALL_DIR/dashboard" ./cmd/dashboard
  cp -r web "$INSTALL_DIR/"
else
  R "找不到源码。请在此项目目录下运行,或先 git clone 后执行。"
  exit 1
fi

# === TLS 证书 ===
CERT_PATH="${CERT:-}"
KEY_PATH="${KEY:-}"

# 优先级: 显式传入证书 > 域名申请 Let's Encrypt > 自签
if [[ -n "$CERT_PATH" && -n "$KEY_PATH" ]]; then
  G "==> 使用已有证书: $CERT_PATH"
  [[ -f "$CERT_PATH" ]] || { R "证书文件不存在: $CERT_PATH"; exit 1; }
  [[ -f "$KEY_PATH" ]] || { R "私钥文件不存在: $KEY_PATH"; exit 1; }
  TLS_ARGS="-tls-cert $CERT_PATH -tls-key $KEY_PATH"
elif [[ -n "$DOMAIN" ]]; then
  G "==> 尝试用 Let's Encrypt 为 $DOMAIN 签发证书"
  if ! command -v certbot >/dev/null 2>&1; then
    Y "安装 certbot..."
    if command -v apt-get >/dev/null 2>&1; then apt-get install -y -qq certbot
    elif command -v yum >/dev/null 2>&1; then yum install -y -q certbot
    fi
  fi
  if certbot certonly --standalone -d "$DOMAIN" --non-interactive --agree-tos -m "admin@${DOMAIN}" --keep-until-expiring 2>/dev/null; then
    CERT="/etc/letsencrypt/live/$DOMAIN/fullchain.pem"
    KEY="/etc/letsencrypt/live/$DOMAIN/privkey.pem"
    TLS_ARGS="-tls-cert $CERT -tls-key $KEY"
    G "Let's Encrypt 证书签发成功"
  else
    Y "Let's Encrypt 签发失败,改用自签证书"
    openssl req -x509 -newkey rsa:2048 -keyout "$DATA_DIR/key.pem" -out "$DATA_DIR/cert.pem" -days 3650 -nodes -subj "/CN=$DOMAIN" 2>/dev/null
    TLS_ARGS="-tls-cert $DATA_DIR/cert.pem -tls-key $DATA_DIR/key.pem"
  fi
else
  G "==> 生成自签证书(建议公网用域名 + Let's Encrypt: DOMAIN=你的域名)"
  openssl req -x509 -newkey rsa:2048 -keyout "$DATA_DIR/key.pem" -out "$DATA_DIR/cert.pem" -days 3650 -nodes -subj "/CN=localhost" 2>/dev/null
  TLS_ARGS="-tls-cert $DATA_DIR/cert.pem -tls-key $DATA_DIR/key.pem"
fi

# === systemd 服务 ===
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
echo "  访问地址:  https://$(hostname -I 2>/dev/null | awk '{print $1}' || echo '服务器IP'):$PORT/?token=$WEB_TOKEN"
echo "  Agent 密钥(secret): $SECRET"
echo "  Web 访问口令(token): $WEB_TOKEN"
echo
Y "  Agent 连接命令(在被监控机器上执行):"
echo "    ./agent -server $(echo ${DOMAIN:-$(hostname -I 2>/dev/null | awk '{print $1}')}):$PORT -token $SECRET -name \"主机名\" -tls -insecure"
echo
echo "  服务管理:"
echo "    systemctl status probe-dashboard"
echo "    journalctl -u probe-dashboard -f"
echo
Y "  请妥善保存上面的密钥,这不会再次显示。"
