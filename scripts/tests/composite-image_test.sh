#!/bin/sh
set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
dockerfile=$repository_root/Dockerfile
provider_compose=$repository_root/deploy/provider.compose.yaml
analytics_compose=$repository_root/deploy/analytics.compose.yaml

for binary in academic-provider academic-analytics academicctl; do
  grep -Fq "/app/$binary" "$dockerfile" || {
    printf '%s\n' "复合镜像缺少 $binary" >&2
    exit 1
  }
done

grep -Fq 'command: ["/app/academic-provider"]' "$provider_compose" || {
  printf '%s\n' 'Provider Compose 未显式选择 Provider 命令' >&2
  exit 1
}
grep -Fq 'entrypoint: ["/app/academicctl"]' "$analytics_compose" || {
  printf '%s\n' 'Analytics 迁移未显式选择 academicctl' >&2
  exit 1
}
grep -Fq 'command: ["/app/academic-analytics"]' "$analytics_compose" || {
  printf '%s\n' 'Analytics Compose 未显式选择 Analytics 命令' >&2
  exit 1
}

printf '%s\n' '复合镜像入口检查通过'
