#!/bin/sh
set -eu

environment=${1:-}
case "$environment" in
  production|review) ;;
  *) echo "用法: $0 production|review" >&2; exit 2 ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
docker_bin=${DOCKER_BIN:-docker}
env_file=${PROVIDER_ENV_FILE:-$repo_root/deploy/provider.$environment.env}
provider_compose_file=${PROVIDER_COMPOSE_FILE:-$repo_root/deploy/compose.yaml}
provider_override_file=${PROVIDER_OVERRIDE_FILE:-$repo_root/deploy/provider.atrust.override.yaml}
health_timeout=${PROVIDER_DEPENDENCY_HEALTH_TIMEOUT:-180}
health_interval=${PROVIDER_DEPENDENCY_HEALTH_INTERVAL:-5}

fail() {
  echo "发布前置检查失败: $*" >&2
  exit 1
}

require_file() {
  [ -f "$1" ] && [ -r "$1" ] || fail "文件不存在或不可读: $1"
}

require_secret_file() {
  [ -f "$1" ] && [ -r "$1" ] && [ -s "$1" ] || fail "Secret 文件不存在、不可读或为空: $1"
}

require_directory() {
  [ -d "$1" ] && [ -r "$1" ] || fail "目录不存在或不可读: $1"
}

require_absolute() {
  case "$2" in
    /*) ;;
    *) fail "$1 必须使用宿主机绝对路径" ;;
  esac
}

env_value() {
  key=$1
  awk -v key="$key" '
    index($0, key "=") == 1 {
      print substr($0, length(key) + 2)
      exit
    }
  ' "$env_file"
}

yaml_value() {
  file=$1
  key=$2
  awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key ":[[:space:]]*" {
      value=$0
      sub("^[[:space:]]*" key ":[[:space:]]*", "", value)
      sub(/[[:space:]]+#.*$/, "", value)
      gsub(/^['\''\"]|['\''\"]$/, "", value)
      print value
      exit
    }
  ' "$file"
}

resolve_setting() {
  variable_name=$1
  file_value=$(env_value "$variable_name")
  process_value=$(printenv "$variable_name" 2>/dev/null || true)
  if [ -n "$process_value" ]; then
    printf '%s\n' "$process_value"
  else
    printf '%s\n' "$file_value"
  fi
}

require_published_port() {
  variable_name=$1
  port=$2
  case "$port" in
    ''|*[!0-9]*) fail "$variable_name 必须是 1 到 65535 的宿主机端口" ;;
  esac
  [ "$port" -ge 1 ] 2>/dev/null && [ "$port" -le 65535 ] 2>/dev/null || \
    fail "$variable_name 必须是 1 到 65535 的宿主机端口"
}

require_role_compose_project() {
  project=$1
  case "$project" in
    campus-academic-"$environment"-provider|campus-academic-"$environment"-provider-?*) ;;
    *) fail "COMPOSE_PROJECT_NAME 必须是 campus-academic-${environment}-provider 或其节点后缀" ;;
  esac
  case "$project" in
    *[!a-z0-9-]*|*--*|-|*-) fail "COMPOSE_PROJECT_NAME 只能包含小写字母、数字和单个连字符" ;;
  esac
}

provider_compose() {
  COMPOSE_PROJECT_NAME="$compose_project_name" "$docker_bin" compose --env-file "$env_file" \
    -f "$provider_compose_file" -f "$provider_override_file" "$@"
}

container_health() {
  "$docker_bin" inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$1"
}

wait_healthy() {
  container=$1
  label=$2
  elapsed=0
  while [ "$elapsed" -le "$health_timeout" ]; do
    status=$("$docker_bin" inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
    health=$(container_health "$container" 2>/dev/null || true)
    if [ "$status" = running ] && [ "$health" = healthy ]; then
      echo "$label 已健康"
      return
    fi
    [ "$status" != exited ] && [ "$status" != dead ] || \
      fail "$label 在等待健康期间退出"
    sleep "$health_interval"
    elapsed=$((elapsed + health_interval))
  done
  fail "$label 未在 ${health_timeout}s 内变为 healthy"
}

require_file "$env_file"
require_file "$provider_compose_file"
require_file "$provider_override_file"

provider_image=$(resolve_setting CAMPUS_ACADEMIC_PROVIDER_IMAGE)
bootstrap_file=$(resolve_setting CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE)
provider_config_file=$(resolve_setting CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE)
rpc_tls_host_dir=$(resolve_setting CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR)
provider_redis_tls_host_dir=$(resolve_setting CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR)
atrust_gateway_home=$(resolve_setting CAMPUS_ATRUST_GATEWAY_HOME)
provider_published_port=$(resolve_setting CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT)
compose_project_name=$(resolve_setting COMPOSE_PROJECT_NAME)
atrust_client_network=$(resolve_setting CAMPUS_ATRUST_CLIENT_NETWORK)

: "${provider_published_port:=9090}"
: "${compose_project_name:=campus-academic-${environment}-provider}"

[ -n "$provider_image" ] || fail "未配置 CAMPUS_ACADEMIC_PROVIDER_IMAGE"
[ -n "$bootstrap_file" ] || fail "未配置 CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE"
[ -n "$provider_config_file" ] || fail "未配置 CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE"
[ -n "$rpc_tls_host_dir" ] || fail "未配置 CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR"
[ -n "$provider_redis_tls_host_dir" ] || fail "未配置 CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR"
[ -n "$atrust_gateway_home" ] || fail "未配置 CAMPUS_ATRUST_GATEWAY_HOME"
[ -n "$atrust_client_network" ] || fail "未配置 CAMPUS_ATRUST_CLIENT_NETWORK"
require_published_port CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT "$provider_published_port"
require_role_compose_project "$compose_project_name"
case "$atrust_client_network" in
  campus-"$environment"-atrust-clients|campus-"$environment"-atrust-clients-?*) ;;
  *) fail "CAMPUS_ATRUST_CLIENT_NETWORK 必须带有 campus-${environment}-atrust-clients 环境前缀" ;;
esac
require_absolute CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE "$bootstrap_file"
require_absolute CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE "$provider_config_file"
require_absolute CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR "$rpc_tls_host_dir"
require_absolute CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR "$provider_redis_tls_host_dir"
require_absolute CAMPUS_ATRUST_GATEWAY_HOME "$atrust_gateway_home"

require_file "$bootstrap_file"
require_file "$provider_config_file"
require_directory "$rpc_tls_host_dir"
require_directory "$provider_redis_tls_host_dir"
require_file "$atrust_gateway_home/scripts/ensure-gateway.sh"
[ -x "$atrust_gateway_home/scripts/ensure-gateway.sh" ] || \
  fail "独立 aTrust 发布器不可执行: $atrust_gateway_home/scripts/ensure-gateway.sh"

[ "$(yaml_value "$bootstrap_file" environment)" = "$environment" ] || \
  fail "bootstrap environment 与发布环境不一致"
[ "$(yaml_value "$bootstrap_file" insecure)" = false ] || \
  fail "Review/Production Provider 必须启用 mTLS"
[ "$(yaml_value "$bootstrap_file" target)" = 127.0.0.1:9090 ] || \
  fail "Provider 容器健康检查 target 必须为 127.0.0.1:9090"
[ "$(yaml_value "$bootstrap_file" tls_files_root)" = /run/secrets/academic-rpc ] || \
  fail "Provider mTLS 根目录必须为 /run/secrets/academic-rpc"
[ "$(yaml_value "$bootstrap_file" http_proxy_url)" = http://atrust-gateway:8888 ] || \
  fail "Provider 必须通过 http://atrust-gateway:8888 访问 OUC"

for tls_key in ca_file client_cert_file client_key_file server_cert_file server_key_file server_name; do
  [ -n "$(yaml_value "$bootstrap_file" "$tls_key")" ] || fail "Provider mTLS 缺少 $tls_key"
done

for tls_key in ca_file client_cert_file client_key_file server_cert_file server_key_file; do
  tls_file=$(yaml_value "$bootstrap_file" "$tls_key")
  case "$tls_file" in */*|..|.) fail "Provider mTLS 的 $tls_key 必须是根目录内文件名" ;; esac
  require_secret_file "$rpc_tls_host_dir/$tls_file"
done

active_provider=$(yaml_value "$provider_config_file" active_provider)
if [ "$environment" = production ]; then
  [ "$active_provider" = ouc ] || fail "Production 禁止使用 Mock Provider"
  case "$provider_image" in *@sha256:*) ;; *) fail "Production Provider 镜像必须使用 sha256 摘要" ;; esac
else
  case "$active_provider" in mock|ouc) ;; *) fail "Review Provider 必须为 mock 或 ouc" ;; esac
fi

"$docker_bin" compose version >/dev/null
provider_compose config --quiet

echo "调用独立 campus-atrust-gateway 依赖发布器"
ATRUST_ENV_FILE="$env_file" "$atrust_gateway_home/scripts/ensure-gateway.sh" "$environment"

echo "开始发布 Academic Provider"
provider_compose up -d --no-deps --no-build academic-provider
provider_container=$(provider_compose ps -q academic-provider)
[ -n "$provider_container" ] || fail "Provider Compose 未返回容器 ID"
wait_healthy "$provider_container" "Academic Provider"

echo "$environment Provider 发布完成；aTrust 与网络依赖已通过幂等检查"
