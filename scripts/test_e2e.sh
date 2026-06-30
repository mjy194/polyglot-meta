#!/bin/bash
# 账号池端到端测试脚本
# 凭证写死在下方 Step 4 的环境变量中（请与 config.yaml 保持同步）

set -e
# 以脚本所在目录为基准定位项目子目录，支持从任意位置调用
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
POLY_DIR="$SCRIPT_DIR/../polyglot/src/srv"
ADAPTER_DIR="$SCRIPT_DIR/../adapter"
POLY_BIN="/tmp/polyglot-server"
ADAPTER_BIN="/tmp/uipath-adapter"
POLY_LOG="/tmp/polyglot.log"
ADAPTER_LOG="/tmp/adapter.log"

POLY_PID=""
ADAPTER_PID=""
claude_rc=0
codex_rc=0
gemini_rc=0

# 打印日志中匹配 PATTERN 的行（最多 LIM 行）；若无匹配则回退打印日志尾部。
# 直接写 `grep ... | head || cat` 会导致回退永不触发：管道退出码取最后一个命令
# （head/tail 即使空输入也返回 0），所以 grep 无匹配时整条管道"成功"，|| cat 被跳过。
show_log() {
  local logfile="$1" pattern="$2" lim="${3:-20}" tailn="${4:-30}"
  local matched
  matched=$(grep -E "$pattern" "$logfile" 2>/dev/null | head -n "$lim")
  if [ -n "$matched" ]; then
    printf '%s\n' "$matched"
  else
    echo "(no lines matched /$pattern/ — showing last $tailn lines)"
    tail -n "$tailn" "$logfile" 2>/dev/null
  fi
}

cleanup() {
  echo "🧹 Cleaning up background processes..."
  # 先 SIGTERM 给优雅关闭；adapter 的 GracefulStop 可能卡在未完成的 SupplyAccounts，
  # 所以 2s 后用 SIGKILL 强杀，避免进程残留继续写日志/打 UiPath。
  for pid in "$POLY_PID" "$ADAPTER_PID"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null || true
  done
  sleep 2
  for pid in "$POLY_PID" "$ADAPTER_PID"; do
    [ -n "$pid" ] && kill -9 "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

echo "=== Step 1: Kill old processes ==="
fuser -k 50051/tcp 50052/tcp 3100/tcp 2>/dev/null || true
sleep 2

echo "=== Step 2: Build ==="
# 在子 shell 中构建，避免 cd 污染主 shell 的工作目录；构建失败时 set -e 仍会中止脚本
(cd "$POLY_DIR" && go build -o "$POLY_BIN" ./cmd/polyglot/) && echo "✅ polyglot built"
(cd "$ADAPTER_DIR" && go build -o "$ADAPTER_BIN" ./cmd/adapter/) && echo "✅ adapter built"

echo "=== Step 3: Start main service ==="
# 必须 cd 到 polyglot 目录，因为二进制用相对路径 configs/config.yaml 加载配置
(cd "$POLY_DIR" && "$POLY_BIN") > "$POLY_LOG" 2>&1 &
POLY_PID=$!
echo "polyglot PID=$POLY_PID"
echo "Waiting for polyglot to be ready..."
HEALTH_TIMEOUT=30
for i in $(seq 1 "$HEALTH_TIMEOUT"); do
  sleep 1
  if curl -sf http://localhost:3100/health >/dev/null 2>&1; then
    echo "✅ polyglot health check passed (${i}s)"
    break
  fi
  if [ "$i" -eq "$HEALTH_TIMEOUT" ]; then
    echo "❌ polyglot failed to start after ${HEALTH_TIMEOUT}s, check $POLY_LOG"
    cat "$POLY_LOG"
    exit 1
  fi
done

echo "=== Step 4: Start adapter with real credentials ==="
UIPATH_EMAIL=abc@uruz.top \
UIPATH_PASSWORD=Yingdaodev1. \
UIPATH_ORG_NAME=uarchqebl \
UIPATH_TENANT_NAME=DefaultTenant \
MAIN_SERVICE_ADDR=127.0.0.1:50052 \
"$ADAPTER_BIN" > "$ADAPTER_LOG" 2>&1 &
ADAPTER_PID=$!
echo "adapter PID=$ADAPTER_PID"

echo "=== Step 5: Wait 15s for registration ==="
sleep 15
echo "--- polyglot log (so far) ---"
show_log "$POLY_LOG" "Registered|watermark|Supplied|Supply|below|ERROR|gRPC"
echo "--- adapter log (so far) ---"
show_log "$ADAPTER_LOG" "Registered|watermark|OAuth|token|ERROR|source|account"

echo "=== Step 6: Wait 90s for OAuth + watermark supply ==="
echo "(watching logs...)"
for i in $(seq 1 9); do
  sleep 10
  echo "--- ${i}0s mark ---"
  recent=$(grep -hE "Registered|watermark|Supplied|Supply|below|OAuth|✅|❌|Created session" "$POLY_LOG" "$ADAPTER_LOG" 2>/dev/null | tail -n 10)
  if [ -n "$recent" ]; then printf '%s\n' "$recent"; else echo "(no matching lines yet)"; fi
done

echo "=== Step 7: Send test message via claude CLI ==="
# 用真实 Claude CLI（Anthropic 兼容客户端）打 gateway，比 curl 更贴近真实使用。
command -v claude >/dev/null 2>&1 || { echo "❌ 未找到 claude CLI，请先安装"; exit 1; }
# 用一个独立临时目录同时充当 cwd 与 CLAUDE_CONFIG_DIR：
#  - 作为 cwd：空目录，避免触发项目信任提示；
#  - 作为 config dir：避免用户 ~/.claude/settings.json 的 "env" 块（例如把
#    ANTHROPIC_BASE_URL 指向别的网关）覆盖我们指向 polyglot 的设置。
# polyglot 现在同时提供标准路径 /v1/messages 与兼容路径 /api/v1/messages。
CLAUDE_RUN_DIR="$(mktemp -d)"
set +e
( cd "$CLAUDE_RUN_DIR" && \
  env -u ANTHROPIC_AUTH_TOKEN \
      ANTHROPIC_BASE_URL=http://localhost:3100 \
      ANTHROPIC_API_KEY=test \
      CLAUDE_CONFIG_DIR="$CLAUDE_RUN_DIR" \
      claude -p "say hi in one word" --model claude-opus-4-8 )
claude_rc=$?
set -e
rm -rf "$CLAUDE_RUN_DIR"
echo ""
if [ "$claude_rc" -ne 0 ]; then
  echo "❌ claude 请求失败 (exit=$claude_rc) — 继续打印日志"
fi

echo "=== Step 8: Send test message via codex CLI ==="
command -v codex >/dev/null 2>&1 || { echo "❌ 未找到 codex CLI，请先安装"; exit 1; }
CODEX_RUN_DIR="$(mktemp -d)"
mkdir -p "$CODEX_RUN_DIR/home"
cat > "$CODEX_RUN_DIR/home/config.toml" <<'EOF'
model_provider = "polyglot"
model = "gpt-4.1"
disable_response_storage = true

[model_providers.polyglot]
name = "polyglot"
base_url = "http://127.0.0.1:3100/v1"
wire_api = "responses"
requires_openai_auth = true
EOF
set +e
( env CODEX_HOME="$CODEX_RUN_DIR/home" \
      OPENAI_API_KEY=test \
      codex exec \
        --skip-git-repo-check \
        --dangerously-bypass-approvals-and-sandbox \
        --ignore-rules \
        -C "$CODEX_RUN_DIR" \
        --output-last-message "$CODEX_RUN_DIR/codex-last.txt" \
        "say hi in one word" )
codex_rc=$?
set -e
cat "$CODEX_RUN_DIR/codex-last.txt" 2>/dev/null || true
rm -rf "$CODEX_RUN_DIR"
echo ""
if [ "$codex_rc" -ne 0 ]; then
  echo "❌ codex 请求失败 (exit=$codex_rc) — 继续打印日志"
fi

echo "=== Step 9: Send test message via gemini CLI ==="
command -v gemini >/dev/null 2>&1 || { echo "❌ 未找到 gemini CLI，请先安装"; exit 1; }
GEMINI_RUN_DIR="$(mktemp -d)"
cat > "$GEMINI_RUN_DIR/settings.json" <<'EOF'
{
  "security": {
    "auth": {
      "selectedType": "gemini-api-key"
    }
  }
}
EOF
set +e
( env GEMINI_CLI_HOME="$GEMINI_RUN_DIR" \
      GEMINI_API_KEY=test \
      GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:3100 \
      GOOGLE_VERTEX_BASE_URL=http://127.0.0.1:3100 \
      gemini -p "say hi in one word" --model gemini-pro --skip-trust --output-format json > "$GEMINI_RUN_DIR/output.json" )
gemini_rc=$?
set -e
cat "$GEMINI_RUN_DIR/output.json" 2>/dev/null || true
rm -rf "$GEMINI_RUN_DIR"
echo ""
if [ "$gemini_rc" -ne 0 ]; then
  echo "❌ gemini 请求失败 (exit=$gemini_rc) — 继续打印日志"
fi

echo "=== Step 10: Final logs ==="
echo "--- polyglot ---"
cat "$POLY_LOG" | tail -50
echo "--- adapter ---"
cat "$ADAPTER_LOG" | tail -50

echo "=== Done ==="

if [ "$claude_rc" -ne 0 ]; then
  exit "$claude_rc"
fi
if [ "$codex_rc" -ne 0 ]; then
  exit "$codex_rc"
fi
if [ "$gemini_rc" -ne 0 ]; then
  exit "$gemini_rc"
fi
