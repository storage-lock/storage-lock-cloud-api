#!/usr/bin/env bash
# 在 Linux VPS 上安装 Storage Lock 云锁服务。
#
#   sudo ./install.sh
#
# 前提：Ubuntu 22.04/24.04 或同级发行版，已装好 nginx（可选）。
# 脚本做的事：
#   1. 创建系统用户与数据/配置目录
#   2. 生成租户主密钥（随机 32 字节）并写入 /etc/storage-lock/cloud-api.env
#   3. 编译并安装二进制
#   4. 安装 systemd unit 并启动
#   5. 给出 nginx/TLS 的下一步提示
set -euo pipefail

APP=storage-lock
BIN=/usr/local/bin/cloud-api
DATA=/var/lib/storage-lock
CONF=/etc/storage-lock
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "请用 root 运行：sudo $0" >&2
  exit 1
fi

echo "==> 创建用户与目录"
id -u "$APP" >/dev/null 2>&1 || useradd --system --shell /usr/sbin/nologin --home "$DATA" "$APP"
install -d -o "$APP" -g "$APP" -m 0750 "$DATA"
install -d -o root -g "$APP" -m 0750 "$CONF"

echo "==> 生成租户主密钥（仅首次）"
if [[ ! -f "$CONF/cloud-api.env" ]]; then
  KEY=$(head -c 32 /dev/urandom | base64)
  umask 077
  printf 'STORAGE_LOCK_TENANT_KEY=%s\n' "$KEY" > "$CONF/cloud-api.env"
  chown root:"$APP" "$CONF/cloud-api.env"
  echo "已写入 $CONF/cloud-api.env（请纳入备份：丢失后所有租户凭据失效）"
else
  echo "$CONF/cloud-api.env 已存在，保留原密钥"
fi

echo "==> 编译"
if ! command -v go >/dev/null; then
  echo "未找到 Go 工具链；请先安装 Go 1.23+ 或改用 deploy/Dockerfile" >&2
  exit 1
fi
cd "$SRC"
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o "$BIN" .
chmod 0755 "$BIN"

echo "==> 安装 systemd unit"
cp "$SRC/deploy/systemd/storage-lock-cloud-api.service" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now storage-lock-cloud-api

sleep 1
if systemctl is-active --quiet storage-lock-cloud-api; then
  echo "==> 服务已启动"
  curl -fsS http://127.0.0.1:8080/health && echo
else
  echo "启动失败，查看日志：journalctl -u storage-lock-cloud-api -n 50" >&2
  exit 1
fi

cat <<'TIP'

下一步（公网接入）：
  1. 把 deploy/nginx.conf 里的 locks.example.com 换成你的域名，放到
     /etc/nginx/sites-available/ 并软链到 sites-enabled/；
  2. 用 certbot 签证书：
       certbot --nginx -d locks.example.com
  3. nginx -t && systemctl reload nginx
  4. 客户端设置：
       export STORAGE_LOCK_CLOUD_URL=https://locks.example.com

注意：网关只监听 127.0.0.1:8080。不要把它直接暴露到公网——
否则没人替它终止 TLS，secret 会以明文传输。
TIP
