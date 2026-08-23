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
env_file=${ANALYTICS_ENV_FILE:-$repo_root/deploy/analytics.$environment.env}
analytics_compose_file=${ANALYTICS_COMPOSE_FILE:-$repo_root/deploy/analytics.compose.yaml}
health_timeout=${ANALYTICS_DEPENDENCY_HEALTH_TIMEOUT:-180}
health_interval=${ANALYTICS_DEPENDENCY_HEALTH_INTERVAL:-5}

fail() { echo "Analytics 发布前置检查失败: $*" >&2; exit 1; }
require_file() { [ -f "$1" ] && [ -r "$1" ] || fail "文件不存在或不可读: $1"; }
require_secret_file() { [ -f "$1" ] && [ -r "$1" ] && [ -s "$1" ] || fail "Secret 文件不存在、不可读或为空: $1"; }
require_directory() { [ -d "$1" ] && [ -r "$1" ] || fail "目录不存在或不可读: $1"; }
require_absolute() { case "$2" in /*) ;; *) fail "$1 必须使用宿主机绝对路径" ;; esac; }

env_value() {
  key=$1
  awk -v key="$key" 'index($0, key "=") == 1 { print substr($0, length(key) + 2); exit }' "$env_file"
}

resolve_setting() {
  variable_name=$1
  file_value=$(env_value "$variable_name")
  process_value=$(printenv "$variable_name" 2>/dev/null || true)
  if [ -n "$process_value" ]; then printf '%s\n' "$process_value"; else printf '%s\n' "$file_value"; fi
}

analytics_value() {
  key=$2
  awk -v key="$key" '
    /^analytics:[[:space:]]*$/ { inside=1; next }
    inside && /^[^[:space:]]/ { exit }
    inside && $0 ~ "^  " key ":[[:space:]]*" {
      value=$0; sub("^  " key ":[[:space:]]*", "", value); sub(/[[:space:]]+#.*$/, "", value); gsub(/^['\''\"]|['\''\"]$/, "", value); print value; exit
    }
  ' "$1"
}

analytics_redis_value() {
  key=$2
  awk -v key="$key" '
    /^analytics:[[:space:]]*$/ { analytics=1; next }
    analytics && /^[^[:space:]]/ { exit }
    analytics && /^  redis:[[:space:]]*$/ { redis=1; next }
    redis && /^  [^[:space:]]/ { exit }
    redis && $0 ~ "^    " key ":[[:space:]]*" {
      value=$0; sub("^    " key ":[[:space:]]*", "", value); sub(/[[:space:]]+#.*$/, "", value); gsub(/^['\''\"]|['\''\"]$/, "", value); print value; exit
    }
  ' "$1"
}

redis_config_value() {
  key=$2
  awk -v key="$key" '
    {
      line=$0
      sub(/^[[:space:]]*/, "", line)
      if (line == "" || substr(line, 1, 1) == "#") next
      split(line, fields, /[[:space:]]+/)
      if (fields[1] == key) {
        print fields[2]
        exit
      }
    }
  ' "$1"
}

analytics_compose() { "$docker_bin" compose --env-file "$env_file" -f "$analytics_compose_file" "$@"; }
container_health() { "$docker_bin" inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$1"; }

wait_healthy() {
  container=$1
  label=$2
  elapsed=0
  while [ "$elapsed" -le "$health_timeout" ]; do
    status=$("$docker_bin" inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
    health=$(container_health "$container" 2>/dev/null || true)
    if [ "$status" = running ] && [ "$health" = healthy ]; then echo "$label 已健康"; return; fi
    [ "$status" != exited ] && [ "$status" != dead ] || fail "$label 在等待健康期间退出"
    sleep "$health_interval"
    elapsed=$((elapsed + health_interval))
  done
  fail "$label 未在 ${health_timeout}s 内变为 healthy"
}

wait_migration() {
  container=$(analytics_compose ps -q analytics-migrate)
  [ -n "$container" ] || fail "Analytics 迁移 Compose 未返回容器 ID"
  status=$("$docker_bin" inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true)
  exit_code=$("$docker_bin" inspect --format '{{.State.ExitCode}}' "$container" 2>/dev/null || true)
  [ "$status" = exited ] && [ "$exit_code" = 0 ] || fail "Analytics 迁移未成功完成"
}

validate_relative_secret_name() {
  label=$1
  file_name=$2
  case "$file_name" in ''|*/*|.|..) fail "$label 必须是 TLS 根目录内文件名" ;; esac
}

redis_tls_host_file() {
  label=$1
  container_path=$2
  case "$container_path" in
    /run/secrets/analytics-redis/*)
      file_name=${container_path#/run/secrets/analytics-redis/}
      validate_relative_secret_name "$label" "$file_name"
      printf '%s\n' "$analytics_redis_tls_host_dir/$file_name"
      ;;
    *) fail "$label 必须位于 /run/secrets/analytics-redis 根目录" ;;
  esac
}

require_file "$env_file"
require_file "$analytics_compose_file"

analytics_image=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_IMAGE)
bootstrap_file=$(resolve_setting CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE)
rpc_tls_host_dir=$(resolve_setting CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR)
analytics_redis_tls_host_dir=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_HOST_DIR)
analytics_redis_config_file=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE)
analytics_redis_ca_env=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE)
analytics_redis_client_cert_env=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE)
analytics_redis_client_key_env=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE)
analytics_redis_server_name_env=$(resolve_setting CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME)

[ -n "$analytics_image" ] || fail "未配置 CAMPUS_ACADEMIC_ANALYTICS_IMAGE"
[ -n "$bootstrap_file" ] || fail "未配置 CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE"
[ -n "$rpc_tls_host_dir" ] || fail "未配置 CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR"
[ -n "$analytics_redis_tls_host_dir" ] || fail "未配置 CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_HOST_DIR"
[ -n "$analytics_redis_config_file" ] || fail "未配置 CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE"
[ -n "$analytics_redis_ca_env" ] || fail "未配置 CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE"
[ -n "$analytics_redis_server_name_env" ] || fail "未配置 CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME"
require_absolute CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE "$bootstrap_file"
require_absolute CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR "$rpc_tls_host_dir"
require_absolute CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_HOST_DIR "$analytics_redis_tls_host_dir"
require_absolute CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE "$analytics_redis_config_file"
require_file "$bootstrap_file"
require_directory "$rpc_tls_host_dir"
require_directory "$analytics_redis_tls_host_dir"
require_file "$analytics_redis_config_file"

[ "$(redis_config_value "$analytics_redis_config_file" port)" = 0 ] || fail "Analytics Redis 必须通过 port 0 关闭明文端口"
[ "$(redis_config_value "$analytics_redis_config_file" tls-port)" = 6379 ] || fail "Analytics Redis tls-port 必须为 6379"
for redis_tls_directive in tls-cert-file tls-key-file tls-ca-cert-file; do
  redis_tls_path=$(redis_config_value "$analytics_redis_config_file" "$redis_tls_directive")
  [ -n "$redis_tls_path" ] || fail "Analytics Redis 配置缺少 $redis_tls_directive"
  redis_tls_file=$(redis_tls_host_file "Analytics Redis 的 $redis_tls_directive" "$redis_tls_path")
  require_secret_file "$redis_tls_file"
done

[ "$(awk -F': ' '$1 == "environment" { print $2; exit }' "$bootstrap_file")" = "$environment" ] || fail "bootstrap environment 与发布环境不一致"
[ "$(analytics_value "$bootstrap_file" insecure)" = false ] || fail "Review/Production Analytics 必须启用 mTLS"
[ "$(analytics_value "$bootstrap_file" target)" = 127.0.0.1:9091 ] || fail "Analytics 容器健康检查 target 必须为 127.0.0.1:9091"
[ "$(analytics_value "$bootstrap_file" tls_files_root)" = /run/secrets/academic-rpc ] || fail "Analytics mTLS 根目录必须为 /run/secrets/academic-rpc"

for tls_key in ca_file client_cert_file client_key_file server_cert_file server_key_file server_name; do
  value=$(analytics_value "$bootstrap_file" "$tls_key")
  [ -n "$value" ] || fail "Analytics mTLS 缺少 $tls_key"
done
for tls_key in ca_file client_cert_file client_key_file server_cert_file server_key_file; do
  tls_file=$(analytics_value "$bootstrap_file" "$tls_key")
  validate_relative_secret_name "Analytics mTLS 的 $tls_key" "$tls_file"
  require_secret_file "$rpc_tls_host_dir/$tls_file"
done

[ "$(analytics_redis_value "$bootstrap_file" tls)" = true ] || fail "Review/Production Analytics Redis 必须启用 TLS"
[ "$(analytics_redis_value "$bootstrap_file" tls_files_root)" = /run/secrets/analytics-redis ] || fail "Analytics Redis TLS 根目录必须为 /run/secrets/analytics-redis"
for tls_key in ca_file server_name; do
  value=$(analytics_redis_value "$bootstrap_file" "$tls_key")
  [ -n "$value" ] || fail "Analytics Redis TLS 缺少 $tls_key"
done
redis_ca_file=$(analytics_redis_value "$bootstrap_file" ca_file)
[ "$analytics_redis_ca_env" = "$redis_ca_file" ] || fail "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE 必须与 bootstrap 一致"
[ "$analytics_redis_server_name_env" = "$(analytics_redis_value "$bootstrap_file" server_name)" ] || fail "CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME 必须与 bootstrap 一致"
validate_relative_secret_name "Analytics Redis TLS 的 ca_file" "$redis_ca_file"
require_secret_file "$analytics_redis_tls_host_dir/$redis_ca_file"
redis_client_cert=$(analytics_redis_value "$bootstrap_file" client_cert_file)
redis_client_key=$(analytics_redis_value "$bootstrap_file" client_key_file)
[ "$analytics_redis_client_cert_env" = "$redis_client_cert" ] || fail "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE 必须与 bootstrap 一致"
[ "$analytics_redis_client_key_env" = "$redis_client_key" ] || fail "CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE 必须与 bootstrap 一致"
[ -z "$redis_client_cert" ] && [ -z "$redis_client_key" ] || {
  [ -n "$redis_client_cert" ] && [ -n "$redis_client_key" ] || fail "Analytics Redis 客户端证书和私钥必须同时配置"
  validate_relative_secret_name "Analytics Redis TLS 的 client_cert_file" "$redis_client_cert"
  validate_relative_secret_name "Analytics Redis TLS 的 client_key_file" "$redis_client_key"
  require_secret_file "$analytics_redis_tls_host_dir/$redis_client_cert"
  require_secret_file "$analytics_redis_tls_host_dir/$redis_client_key"
}

case "$analytics_image" in *@sha256:[0-9a-f][0-9a-f]*) digest=${analytics_image##*@sha256:} ;; *) digest= ;; esac
if [ "$environment" = production ]; then
  [ "${#digest}" -eq 64 ] 2>/dev/null && case "$digest" in *[!0-9a-f]*) false ;; *) true ;; esac || fail "Production Analytics 镜像必须使用 64 位 sha256 摘要"
elif [ -z "$digest" ]; then
  echo "Review 警告：建议使用 CAMPUS_ACADEMIC_ANALYTICS_IMAGE 的不可变 sha256 摘要" >&2
fi

"$docker_bin" compose version >/dev/null
analytics_compose config --quiet

echo "开始启动 Analytics MySQL 与 Redis"
analytics_compose up -d --no-build analytics-mysql analytics-redis
for service in analytics-mysql analytics-redis; do
  container=$(analytics_compose ps -q "$service")
  [ -n "$container" ] || fail "$service Compose 未返回容器 ID"
  wait_healthy "$container" "$service"
done

echo "开始执行 Analytics 迁移"
analytics_compose up --no-build --no-deps --abort-on-container-exit analytics-migrate
wait_migration

echo "开始发布 Academic Analytics"
analytics_compose up -d --no-build --no-deps academic-analytics
analytics_container=$(analytics_compose ps -q academic-analytics)
[ -n "$analytics_container" ] || fail "Analytics Compose 未返回容器 ID"
wait_healthy "$analytics_container" "Academic Analytics gRPC healthcheck"

echo "$environment Analytics 发布完成；未启动 Provider、aTrust 或 Provider Redis"
