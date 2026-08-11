#!/usr/bin/env bash
# sync-upstream.sh — 同步官方上游到本地 main-v2 并推送 fork
#
# 设计：
#   - 无冲突：自动 merge + 编译验证 + 推送到 fork，一条命令完成
#   - 有冲突：停在冲突状态，打印处理指引，等待人工审核解决
#   - 网络（GitHub 间歇性不可用）：fetch/push 自动重试
#
# 用法：bash sync-upstream.sh
set -euo pipefail

# ── 配置 ──────────────────────────────────────────────────────────────
ORIGIN=origin        # 官方 esengine/DeepSeek-Reasonix
FORK=mine            # 你的 fork assesvgs/DeepSeek-Reasonix
BRANCH=main-v2
MAX_RETRY=5          # GitHub 网络重试次数
RETRY_DELAY=8        # 重试间隔（秒）

# ── 带重试的 git fetch ────────────────────────────────────────────────
fetch_with_retry() {
  local i
  for i in $(seq 1 "$MAX_RETRY"); do
    echo "  [fetch] 第 $i 次尝试 ..."
    if git fetch "$ORIGIN" 2>&1; then
      return 0
    fi
    echo "  [fetch] 失败（网络问题？），${RETRY_DELAY}s 后重试"
    sleep "$RETRY_DELAY"
  done
  echo "❌ [fetch] 连续 $MAX_RETRY 次失败，请检查网络后重试"
  exit 1
}

# ── 主流程 ─────────────────────────────────────────────────────────────
cd "$(git rev-parse --show-toplevel)"

echo "==> 1/5 拉取官方更新"
fetch_with_retry

echo "==> 2/5 检查落后情况"
BEHIND=$(git rev-list --count "HEAD..$ORIGIN/$BRANCH" 2>/dev/null || echo "?")
if [ "$BEHIND" = "?" ]; then
  echo "❌ 无法比较（分支 $ORIGIN/$BRANCH 不存在？），请检查 remote 配置"
  exit 1
fi
if [ "$BEHIND" -eq 0 ]; then
  echo "✅ 已是最新，落后 0 个提交，无需同步"
  exit 0
fi
echo "    落后官方 $BEHIND 个提交，开始合并"

if [ -n "$(git status --porcelain)" ]; then
  echo "❌ 工作区有未提交改动，请先处理，否则会污染 merge："
  git status --short
  exit 1
fi

echo "==> 3/5 合并 $ORIGIN/$BRANCH"
if git merge "$ORIGIN/$BRANCH" --no-edit 2>&1; then
  echo "    ✅ 合并成功，无冲突"
else
  echo ""
  echo "⚠️  合并产生冲突，已停在冲突状态，等待人工审核："
  echo ""
  echo "    查看冲突文件：git diff --name-only --diff-filter=U"
  echo "    解决后提交：   git add <文件> && git commit"
  echo "    放弃本次合并： git merge --abort"
  echo ""
  echo "    提示：本地 Termux 修复集中在 internal/cli、internal/config、"
  echo "          internal/skill；官方重写过的文件（如 tui_diagnostics.go）"
  echo "          应保留官方新结构、把修复移植进去，而不是把官方代码并回旧结构。"
  exit 1
fi

echo "==> 4/5 编译验证"
if ! go build ./... 2>&1; then
  echo "⚠️  编译失败。merge commit 已生成，可回退：git reset --hard ORIG_HEAD"
  echo "    或修复编译问题后继续：git commit"
  exit 1
fi
echo "    ✅ 编译通过"

echo "==> 5/5 推送到 fork（$FORK/$BRANCH）"
for i in $(seq 1 "$MAX_RETRY"); do
  echo "    [push] 第 $i 次尝试 ..."
  if git push "$FORK" "$BRANCH" 2>&1; then
    echo ""
    echo "🎉 同步完成：本地与 fork 均已更新到官方最新"
    echo "    落后检查：git rev-list --count main-v2..origin/main-v2"
    exit 0
  fi
  echo "    [push] 失败（网络问题？），${RETRY_DELAY}s 后重试"
  sleep "$RETRY_DELAY"
done
echo "⚠️  推送连续失败。本地已合并好，稍后手动执行：git push $FORK $BRANCH"
exit 1
