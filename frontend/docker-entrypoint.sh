#!/bin/sh

# 生成运行时配置文件，注入环境变量到前端
cat > /usr/share/nginx/html/config.js << EOF
window.__RUNTIME_CONFIG__ = {
  MAX_FILE_SIZE_MB: ${MAX_FILE_SIZE_MB:-50},
  MAX_KNOWLEDGE_SOURCE_FILE_SIZE_MB: ${MAX_KNOWLEDGE_SOURCE_FILE_SIZE_MB:-2048}
};
EOF

if [ -d /usr/share/nginx/html/mobile ]; then
  cp /usr/share/nginx/html/config.js /usr/share/nginx/html/mobile/config.js
fi

# 处理 nginx 配置
export MAX_FILE_SIZE=${MAX_FILE_SIZE_MB:-50}M
export MAX_KNOWLEDGE_SOURCE_FILE_SIZE=${MAX_KNOWLEDGE_SOURCE_FILE_SIZE_MB:-2048}M
# Keep user-visible/raw-file limits separate from reverse-proxy request-body
# ceilings. Multipart framing and Base64 JSON transport add overhead before the
# backend can enforce the real 50 MiB / 2 GiB file limits.
export PROXY_MAX_BODY_SIZE=${PROXY_MAX_BODY_SIZE_MB:-80}M
export PROXY_MAX_KNOWLEDGE_SOURCE_BODY_SIZE=${PROXY_MAX_KNOWLEDGE_SOURCE_BODY_SIZE_MB:-2304}M
export PROXY_TIMEOUT=${PROXY_TIMEOUT_SECONDS:-3600}s
export APP_HOST=${APP_HOST:-app}
export APP_PORT=${APP_PORT:-8080}
export APP_SCHEME=${APP_SCHEME:-http}
envsubst '${PROXY_MAX_BODY_SIZE} ${PROXY_MAX_KNOWLEDGE_SOURCE_BODY_SIZE} ${PROXY_TIMEOUT} ${APP_HOST} ${APP_PORT} ${APP_SCHEME}' < /etc/nginx/templates/default.conf.template > /etc/nginx/conf.d/default.conf

# 启动 nginx
exec nginx -g 'daemon off;'
