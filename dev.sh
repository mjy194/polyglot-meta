#!/bin/bash
# dev.sh — 本地开发启动脚本：构建并启动 polyglot + uipath_adapter
# 用法: ./dev.sh

set -e

# 以脚本所在目录为基准定位项目子目录
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
POLY_DIR="$SCRIPT_DIR/polyglot/src/srv"
ADAPTER_DIR="$SCRIPT_DIR/adapter"

POLY_BIN="/tmp/polyglot-server"
ADAPTER_BIN="/tmp/uipath-adapter"
POLY_LOG="/tmp/polyglot.log"
ADAPTER_LOG="/tmp/adapter.log"

POLY_PID=""
ADAPTER_PID=""

cleanup() {
  echo ""
  echo "🛑 Shutting down services..."
  [ -n "$ADAPTER_PID" ] && kill "$ADAPTER_PID" 2>/dev/null || true
  [ -n "$POLY_PID" ] && kill "$POLY_PID" 2>/dev/null || true
  sleep 1
  [ -n "$ADAPTER_PID" ] && kill -9 "$ADAPTER_PID" 2>/dev/null || true
  [ -n "$POLY_PID" ] && kill -9 "$POLY_PID" 2>/dev/null || true
  echo "✅ Done"
}
trap cleanup EXIT INT TERM

echo "=== Building polyglot ==="
(cd "$POLY_DIR" && go build -o "$POLY_BIN" ./cmd/polyglot/) && echo "✅ polyglot built"

echo "=== Building adapter ==="
(cd "$ADAPTER_DIR" && go build -o "$ADAPTER_BIN" ./cmd/adapter/) && echo "✅ adapter built"

echo "=== Starting polyglot ==="
# 必须 cd 到 polyglot 目录，因为二进制用相对路径 configs/config.yaml 加载配置
(cd "$POLY_DIR" && "$POLY_BIN") > "$POLY_LOG" 2>&1 &
POLY_PID=$!
echo "polyglot PID=$POLY_PID  log=$POLY_LOG"

echo "Waiting for polyglot health check..."
for i in $(seq 1 30); do
  sleep 1
  if curl -sf http://localhost:3100/health >/dev/null 2>&1; then
    echo "✅ polyglot ready (${i}s)"
    break
  fi
  if [ "$i" -eq 30 ]; then
    echo "❌ polyglot failed to start, check $POLY_LOG"
    tail -30 "$POLY_LOG"
    exit 1
  fi
done

echo "=== Starting adapter ==="
UIPATH_EMAIL=abc@uruz.top \
UIPATH_PASSWORD=Yingdaodev1. \
UIPATH_ORG_NAME=uarchqebl \
UIPATH_TENANT_NAME=DefaultTenant \
MAIN_SERVICE_ADDR=127.0.0.1:50052 \
"$ADAPTER_BIN" > "$ADAPTER_LOG" 2>&1 &
ADAPTER_PID=$!
echo "adapter PID=$ADAPTER_PID  log=$ADAPTER_LOG"

echo ""
echo "🚀 Both services starting in background..."
echo "   polyglot:  http://localhost:3100  (log: $POLY_LOG)"
echo "   adapter:   gRPC auto-port          (log: $ADAPTER_LOG)"
echo ""
echo "Press Ctrl+C to stop"
echo ""

# 实时显示两个服务的日志
tail -f "$POLY_LOG" "$ADAPTER_LOG" 2>/dev/null
