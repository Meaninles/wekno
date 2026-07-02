#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f ".env" ]; then
    set -a
    # shellcheck source=/dev/null
    source .env
    set +a
fi

export GIN_MODE="${GIN_MODE:-debug}"
export DB_HOST="${DEV_DOCKER_DB_HOST:-postgres}"
export DB_PORT="${DEV_DOCKER_DB_PORT:-5432}"
export DOCREADER_ADDR="${DEV_DOCKER_DOCREADER_ADDR:-docreader:50051}"
export DOCREADER_TRANSPORT="${DOCREADER_TRANSPORT:-grpc}"
export MINIO_ENDPOINT="${DEV_DOCKER_MINIO_ENDPOINT:-minio:9000}"
export REDIS_ADDR="${DEV_DOCKER_REDIS_ADDR:-redis:6379}"
export MILVUS_ADDRESS="${DEV_DOCKER_MILVUS_ADDRESS:-milvus:19530}"
export NEO4J_URI="${DEV_DOCKER_NEO4J_URI:-bolt://neo4j:7687}"
export QDRANT_HOST="${DEV_DOCKER_QDRANT_HOST:-qdrant}"
export LOCAL_STORAGE_BASE_DIR="${DEV_DOCKER_LOCAL_STORAGE_BASE_DIR:-/workspace/.local-data/files}"
export DUCKDB_SKIP_EXTENSION_LOAD="${DUCKDB_SKIP_EXTENSION_LOAD:-true}"
export CGO_ENABLED="${CGO_ENABLED:-1}"
export CGO_CFLAGS="${CGO_CFLAGS:--Wno-deprecated-declarations -Wno-gnu-folding-constant}"

mkdir -p "$LOCAL_STORAGE_BASE_DIR"

LDFLAGS="$(./scripts/get_version.sh ldflags) -X 'google.golang.org/protobuf/reflect/protoregistry.conflictPolicy=warn'"

echo "[INFO] Starting WeKnora backend in Docker dev mode..."
echo "[INFO] DB: ${DB_HOST}:${DB_PORT}"
echo "[INFO] Redis: ${REDIS_ADDR}"
echo "[INFO] DocReader: ${DOCREADER_ADDR}"

exec go run -ldflags="$LDFLAGS" ./cmd/server
