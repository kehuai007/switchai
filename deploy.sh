#!/usr/bin/env bash
# 更新 switchai：停服务 -> 备份 -> 替换二进制 -> 启动
# 用法：
#   cd /home/misu/switchai
#   bash build.sh          # 先以普通用户构建 dist，避免 dist 被 root 拥有
#   sudo ./deploy.sh       # 或直接 ./deploy.sh，脚本会自动 sudo
set -euo pipefail

SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$REPO_DIR/dist/switchai-linux-amd64"
DST="/usr/local/bin/switchai"
SERVICE="switchai"

# 非 root 自动提权
if [[ "${EUID}" -ne 0 ]]; then
    exec sudo -- "$SCRIPT_PATH" "$@"
fi

if [[ ! -f "$SRC" ]]; then
    echo "❌ 找不到新二进制: $SRC"
    echo "请先以普通用户运行: cd $REPO_DIR && bash build.sh"
    exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
    echo "❌ 当前环境没有 systemctl"
    exit 1
fi

BACKUP=""
INSTALL_OK=0

restore_on_error() {
    if [[ "$INSTALL_OK" -eq 1 ]]; then
        return
    fi
    echo "❌ 更新未完成，尝试回滚并启动旧版本..."
    if [[ -n "$BACKUP" && -f "$BACKUP" ]]; then
        cp -a "$BACKUP" "$DST"
    fi
    systemctl start "$SERVICE" >/dev/null 2>&1 || true
    echo "已尝试恢复服务，请检查: systemctl status $SERVICE"
}
trap restore_on_error EXIT

echo "==> 1/4 停止 $SERVICE"
systemctl stop "$SERVICE" || true

# 等待进程真正退出，避免 Text file busy
for _ in $(seq 1 40); do
    if ! systemctl is-active --quiet "$SERVICE"; then
        break
    fi
    sleep 0.25
done

if systemctl is-active --quiet "$SERVICE"; then
    echo "⚠️ 服务仍未停止，强制发送 TERM..."
    systemctl kill -s TERM "$SERVICE" || true
    sleep 2
fi

echo "==> 2/4 备份旧二进制"
if [[ -f "$DST" ]]; then
    BACKUP="$DST.bak.$(date +%Y%m%d-%H%M%S)"
    cp -a "$DST" "$BACKUP"
    echo "    备份: $BACKUP"
else
    echo "    未发现旧二进制，跳过备份"
fi

echo "==> 3/4 安装新二进制"
TMP="$(mktemp "${DST}.new.XXXXXX")"
cp "$SRC" "$TMP"
chmod 0755 "$TMP"
# mv 是原子替换，即使旧进程尚未完全退出也不会 Text file busy
mv -f "$TMP" "$DST"
echo "    已更新: $DST"

echo "==> 4/4 启动 $SERVICE"
if ! systemctl start "$SERVICE"; then
    echo "❌ 启动失败"
    systemctl status "$SERVICE" --no-pager -l | head -30 || true
    echo "---- 最近日志 ----"
    journalctl -u "$SERVICE" -n 30 --no-pager || true
    exit 1
fi

INSTALL_OK=1
trap - EXIT

sleep 1
if systemctl is-active --quiet "$SERVICE"; then
    echo "✅ $SERVICE 已启动"
else
    echo "⚠️ $SERVICE 状态异常"
    systemctl status "$SERVICE" --no-pager -l | head -30 || true
    exit 1
fi

if command -v curl >/dev/null 2>&1; then
    CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:7777/ || true)"
    echo "    首页 HTTP 状态: ${CODE:-连接失败}"
fi

systemctl status "$SERVICE" --no-pager -l | head -15 || true
echo "完成。浏览器请按 Ctrl+F5 强刷管理页。"
