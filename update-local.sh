#!/usr/bin/env bash
# Build the current checkout without sending private runtime data in .data to
# Docker, then recreate only the local platform container from the new image.
set -euo pipefail

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"

fail() {
  printf '\n✗ %s\n' "$1" >&2
  exit 1
}

[[ -r .env ]] || fail '找不到可读的 .env。请先在项目目录完成 LoopForge 配置。'
command -v rsync >/dev/null 2>&1 || fail '需要 rsync。请先安装 rsync 后重试。'
command -v docker >/dev/null 2>&1 || fail '找不到 Docker 命令。'
source scripts/docker-access.sh
select_docker_access || fail 'Docker 未运行，或当前用户无权访问 Docker。'
docker_access compose version >/dev/null 2>&1 || fail 'Docker Compose 不可用。'

TEMP_SOURCE="$(mktemp -d "${TMPDIR:-/tmp}/loopforge-build.XXXXXX")"
cleanup() { rm -rf -- "$TEMP_SOURCE"; }
trap cleanup EXIT

printf '▸ 准备临时源码副本（跳过 .data、.env 和 .git）\n'
rsync -a \
  --exclude='/.data/' \
  --exclude='/.git/' \
  --exclude='/.env' \
  --exclude='/.env.*' \
  "$ROOT/" "$TEMP_SOURCE/"

export LOOPFORGE_COMMIT="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || true)"
printf '▸ 从临时副本构建本机 LoopForge 镜像\n'
docker_access compose --env-file "$ROOT/.env" \
  -f "$TEMP_SOURCE/compose.yaml" \
  -f "$TEMP_SOURCE/compose.dev.yaml" \
  --profile platform build platform \
  || fail '镜像构建失败；当前运行中的容器未被替换。'

printf '▸ 使用新镜像更新本机平台容器（保留数据库和 .data）\n'
docker_access compose --env-file "$ROOT/.env" \
  -f "$ROOT/compose.yaml" \
  -f "$ROOT/compose.dev.yaml" \
  --profile platform up -d --no-build --wait --wait-timeout 180 platform \
  || fail '镜像已构建，但容器更新未完成。查看原因：docker compose --profile platform logs --since 5m platform'

printf '\n✓ 本机平台已更新。查看状态：docker compose --profile platform ps\n'
