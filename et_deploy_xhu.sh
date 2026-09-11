#!/bin/bash

# 基线核查客户端一键部署脚本（全自动版）

echo "=== 基线核查客户端一键部署 ==="

# 检查root权限
if [ "$EUID" -ne 0 ]; then
    echo "❌ 请使用root权限运行此脚本: sudo $0"
    exit 1
fi

# 安装目录
INSTALL_DIR="/opt/et_tools/linux_check"

if [[ "$INSTALL_DIR" != "/opt/"* ]]; then
    echo "❌ 安装路径不合法，必须位于 /opt/ 目录下"
    exit 1
fi

echo "📁 处理安装目录: $INSTALL_DIR"

# 自动处理目录冲突（备份）
if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
    echo "🔄 备份现有目录"
    BACKUP_DIR="${INSTALL_DIR}.backup.$(date +%Y%m%d_%H%M%S)"
    mv "$INSTALL_DIR" "$BACKUP_DIR"
    echo "✅ 备份完成: $BACKUP_DIR"
fi

# 创建安装目录
mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

# 下载客户端
echo "📥 下载客户端..."

if command -v wget >/dev/null 2>&1; then
    wget --no-check-certificate --header="Platform: linux" -O "client_package.zip" "https://10.13.8.132:3331/download/client/latest?platform=linux"
elif command -v curl >/dev/null 2>&1; then
    curl -s -k -L -H "Platform: linux" -o "client_package.zip" "https://10.13.8.132:3331/download/client/latest?platform=linux"
else
    echo "❌ 未找到 wget 或 curl，无法下载客户端。" >&2
    exit 1
fi

# 验证下载
if [ ! -f "client_package.zip" ] || [ ! -s "client_package.zip" ]; then
    echo "❌ 下载失败"
    cd /
    rm -rf "$INSTALL_DIR"
    exit 1
fi

# 解压文件
echo "📦 解压客户端..."
if ! unzip -q "client_package.zip" -d .; then
    echo "❌ 解压失败"
    cd /
    rm -rf "$INSTALL_DIR"
    exit 1
fi

# 设置权限
echo "🔧 设置执行权限..."
find . -name "*.sh" -exec chmod +x {} \; 2>/dev/null || true

# 执行自动化初始化
echo "🚀 执行自动化初始化..."
if [ -f "setup_client.sh" ]; then
    # 传递自动化参数给初始化脚本
    # ./setup_client.sh --server-url "https://10.13.8.132:3331"

	SYSTEM_NAME_ARG="$1"

	if [ -n "$SYSTEM_NAME_ARG" ]; then
    	./setup_client.sh --server-url "https://10.13.8.132:3331" --system-name "$SYSTEM_NAME_ARG"
	else
    	./setup_client.sh --server-url "https://10.13.8.132:3331"
	fi

else
    echo "⚠️  未找到初始化脚本"
fi

echo "✅ 部署完成！安装目录: $INSTALL_DIR"
