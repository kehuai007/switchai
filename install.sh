#!/bin/bash
set -e

REPO="kehuai007/switchai"
INSTALL_DIR="/usr/local/bin"
SERVICE_NAME="switchai"
BINARY_NAME="switchai-linux-amd64"

# 获取最新 Release 版本和下载链接
echo "获取最新版本信息..."
LATEST_TAG=$(curl -s "https://api.github.com/repos/${REPO}/releases/latest" | grep -o '"tag_name": "[^"]*"' | cut -d'"' -f4)
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST_TAG}/${BINARY_NAME}"

echo "最新版本: ${LATEST_TAG}"
echo "下载链接: ${DOWNLOAD_URL}"

# 下载二进制文件
TEMP_FILE="/tmp/${BINARY_NAME}"
echo "下载中..."
curl -L -o "${TEMP_FILE}" "${DOWNLOAD_URL}"

# 验证文件
if [ ! -s "${TEMP_FILE}" ]; then
    echo "下载失败，文件为空"
    exit 1
fi

echo "安装到 ${INSTALL_DIR}..."
chmod +x "${TEMP_FILE}"
mv "${TEMP_FILE}" "${INSTALL_DIR}/${SERVICE_NAME}"

# 创建 systemd 服务文件
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
echo "创建 systemd 服务..."
cat > "${SERVICE_FILE}" << EOF
[Unit]
Description=SwitchAI - Claude API Proxy Service
After=network.target

[Service]
Type=simple
ExecStart=${INSTALL_DIR}/${SERVICE_NAME}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# 重载 systemd 并启动服务
echo "启动服务..."
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}"
systemctl start "${SERVICE_NAME}"

echo ""
echo "✅ 安装完成！"
echo "   服务名称: ${SERVICE_NAME}"
echo "   访问地址: http://localhost:7777"
echo ""
echo "管理命令:"
echo "  systemctl start ${SERVICE_NAME}   # 启动"
echo "  systemctl stop ${SERVICE_NAME}    # 停止"
echo "  systemctl restart ${SERVICE_NAME} # 重启"
echo "  systemctl status ${SERVICE_NAME}  # 状态"
