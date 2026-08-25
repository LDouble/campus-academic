#!/bin/sh
set -eu

environment=${1:-}
state_root=${CAMPUS_DEPLOY_STATE_DIR:-$HOME/.local/share/campus/deploy}
repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
case "$environment" in review|production) ;; *) printf '%s\n' 'usage: prepare-bootstrap.sh review|production' >&2; exit 2 ;; esac

root=$state_root/$environment
bootstrap=$root/bootstrap.env
inputs=$root/inputs.env
managed=$root/managed.env
certs=$root/certs
for file in "$bootstrap" "$inputs" "$managed"; do [ -r "$file" ] || { printf '%s\n' "缺少初始化配置: $file" >&2; exit 1; }; done

value() {
	file=$1 key=$2
	count=$(awk -F= -v key="$key" '$1 == key { n++ } END { print n + 0 }' "$file")
	[ "$count" -le 1 ] || { printf '%s\n' "重复配置: $key" >&2; exit 1; }
	sed -n "s/^${key}=//p" "$file"
}
required() { result=$(value "$1" "$2"); [ -n "$result" ] || { printf '%s\n' "缺少配置: $2" >&2; exit 1; }; printf %s "$result"; }
copy_rpc() {
	destination=$1 prefix=$2
	mkdir -p "$destination"
	cp "$certs/academic-rpc/ca.crt" "$destination/ca.crt"
	cp "$certs/academic-rpc/client.crt" "$destination/client.crt"
	cp "$certs/academic-rpc/client.key" "$destination/client.key"
	cp "$certs/academic-rpc/$prefix.crt" "$destination/server.crt"
	cp "$certs/academic-rpc/$prefix.key" "$destination/server.key"
}

provider_files=$root/roles/provider/files
analytics_files=$root/roles/analytics/files
rm -rf "$provider_files" "$analytics_files"
mkdir -p "$provider_files/provider-redis" "$analytics_files/analytics-redis"
copy_rpc "$provider_files/academic-rpc" provider-server
copy_rpc "$analytics_files/academic-rpc" analytics-server
cp "$certs/provider-redis/ca.crt" "$provider_files/provider-redis/ca.crt"
cp "$certs/provider-redis/client.crt" "$provider_files/provider-redis/client.crt"
cp "$certs/provider-redis/client.key" "$provider_files/provider-redis/client.key"
cp "$certs/provider-redis/server.crt" "$provider_files/provider-redis/server.crt"
cp "$certs/provider-redis/server.key" "$provider_files/provider-redis/server.key"
if [ -s "$certs/provider-redis-external/ca.crt" ]; then
	mkdir -p "$provider_files/provider-redis-external"
	cp "$certs/provider-redis-external/ca.crt" "$provider_files/provider-redis-external/ca.crt"
	[ ! -s "$certs/provider-redis-external/client.crt" ] || cp "$certs/provider-redis-external/client.crt" "$provider_files/provider-redis-external/client.crt"
	[ ! -s "$certs/provider-redis-external/client.key" ] || cp "$certs/provider-redis-external/client.key" "$provider_files/provider-redis-external/client.key"
fi
cp "$certs/analytics-redis/ca.crt" "$analytics_files/analytics-redis/ca.crt"
cp "$certs/analytics-redis/server.crt" "$analytics_files/analytics-redis/server.crt"
cp "$certs/analytics-redis/server.key" "$analytics_files/analytics-redis/server.key"

cat >"$provider_files/bootstrap.yaml" <<EOF
environment: $environment
release: bootstrap
provider_config_file: /app/provider-config.yaml
provider:
  listen_address: ":9090"
  target: 127.0.0.1:9090
  http_proxy_url: http://atrust-gateway:8888
  insecure: false
  tls_files_root: /run/secrets/academic-rpc
  ca_file: ca.crt
  client_cert_file: client.crt
  client_key_file: client.key
  server_cert_file: server.crt
  server_key_file: server.key
  server_name: $(required "$bootstrap" CAMPUS_PROVIDER_TLS_SERVER_NAME)
  max_concurrent: 32
  queue_wait: 250ms
  retry_after: 2s
redis:
  address: provider-redis:6379
  username: default
  password: ""
  db: 0
  tls: true
  tls_files_root: /run/secrets/provider-redis
  ca_file: ca.crt
  client_cert_file: client.crt
  client_key_file: client.key
  server_name: provider-redis.internal
academic_query:
  cache_mode: normal
  courses_timeout: 10s
  grades_timeout: 10s
  exams_timeout: 12s
  selections_timeout: 12s
  stale_refresh_timeout: 2s
  circuit_failure_threshold: 3
  circuit_window: 30s
  circuit_open_duration: 15s
  circuit_minimum_samples: 10
  circuit_deadline_threshold: 3
  circuit_deadline_ratio: 0.20
  circuit_hard_protection_count: 10
  circuit_hard_protection_window: 30s
  lease_ttl: 35s
  poll_interval: 50ms
  global_rate: 10
  global_burst: 20
  courses_fresh_ttl: 5m
  courses_stale_ttl: 30m
  grades_fresh_ttl: 2m
  grades_stale_ttl: 10m
  exams_fresh_ttl: 3m
  exams_stale_ttl: 15m
  selections_fresh_ttl: 30s
  selections_stale_ttl: 5m
observability:
  metrics_address: ":9300"
EOF
cp "$provider_files/bootstrap.yaml" "$analytics_files/bootstrap.yaml"
cat >>"$analytics_files/bootstrap.yaml" <<EOF
analytics:
  listen_address: ":9091"
  target: 127.0.0.1:9091
  insecure: false
  tls_files_root: /run/secrets/academic-rpc
  ca_file: ca.crt
  client_cert_file: client.crt
  client_key_file: client.key
  server_cert_file: server.crt
  server_key_file: server.key
  server_name: $(required "$bootstrap" CAMPUS_ANALYTICS_TLS_SERVER_NAME)
  mysql:
    dsn: ""
  redis:
    address: analytics-redis:6379
    username: default
    password: ""
    db: 0
    tls: true
    tls_files_root: /run/secrets/analytics-redis
    ca_file: ca.crt
    client_cert_file: ""
    client_key_file: ""
    server_name: analytics-redis.internal
  timezone: Asia/Shanghai
  schedule_hour: 4
  minimum_sample_size: 5
  query_timeout: 20m
  worker_concurrency: 2
  task_queue: academic_analytics
  task_timeout: 30m
EOF

provider=$(value "$inputs" CAMPUS_PROVIDER_ACTIVE_PROVIDER); provider=${provider:-ouc}
CAMPUS_PROVIDER_ACTIVE_PROVIDER=$provider CAMPUS_ACADEMIC_PROVIDER_CONFIG_SOURCE=${CAMPUS_ACADEMIC_PROVIDER_CONFIG_SOURCE:-} \
	"$repo_root/scripts/render-provider-config.sh" "$environment" "$provider_files/provider-config.yaml"
: >"$provider_files/atrust-username"; : >"$provider_files/atrust-password"
cat >"$analytics_files/redis.conf" <<'EOF'
port 0
tls-port 6379
tls-cert-file /run/secrets/analytics-redis/server.crt
tls-key-file /run/secrets/analytics-redis/server.key
tls-ca-cert-file /run/secrets/analytics-redis/ca.crt
tls-auth-clients no
appendonly yes
EOF

remote=$(required "$bootstrap" CAMPUS_REMOTE_DEPLOY_ROOT)
provider_current=$remote/bootstrap/$environment/provider/current/files
redis_address=$(value "$inputs" CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS)
if [ -n "$redis_address" ]; then
	redis_mode=external; redis_user=$(required "$inputs" CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME); redis_db=$(required "$inputs" CAMPUS_ACADEMIC_PROVIDER_REDIS_DB); redis_name=$(required "$inputs" CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME)
	redis_cert=$([ -s "$certs/provider-redis-external/client.crt" ] && printf client.crt || true); redis_key=$([ -s "$certs/provider-redis-external/client.key" ] && printf client.key || true)
else
	redis_mode=embedded; redis_address=provider-redis:6379; redis_user=default; redis_db=0; redis_name=provider-redis.internal; redis_cert=client.crt; redis_key=client.key
fi
cat >"$root/roles/provider/generated.env" <<EOF
CAMPUS_ATRUST_GATEWAY_HOME=/opt/campus-atrust-gateway
CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE=$provider_current/bootstrap.yaml
CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE=$provider_current/provider-config.yaml
CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR=$provider_current/academic-rpc
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR=$provider_current/provider-redis
CAMPUS_ATRUST_USERNAME_FILE=$provider_current/atrust-username
CAMPUS_ATRUST_PASSWORD_FILE=$provider_current/atrust-password
CAMPUS_ATRUST_EGRESS_NETWORK=campus-$environment-atrust-egress
CAMPUS_ACADEMIC_PROVIDER_BIND_ADDRESS=0.0.0.0
CAMPUS_ACADEMIC_PROVIDER_KEY=$(required "$managed" CAMPUS_ACADEMIC_PROVIDER_KEY)
CAMPUS_ACADEMIC_QUERY_KEY=$(required "$managed" CAMPUS_ACADEMIC_QUERY_KEY)
CAMPUS_PROVIDER_REDIS_MODE=$redis_mode
CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS=$redis_address
CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME=$redis_user
CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD=$(required "$managed" CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD)
CAMPUS_ACADEMIC_PROVIDER_REDIS_DB=$redis_db
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS=true
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_FILES_ROOT=/run/secrets/provider-redis
CAMPUS_ACADEMIC_PROVIDER_REDIS_CA_FILE=ca.crt
CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_CERT_FILE=$redis_cert
CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_KEY_FILE=$redis_key
CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME=$redis_name
EOF
analytics_current=$remote/bootstrap/$environment/analytics/current/files
source_dsn=$(value "$inputs" CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN)
if [ -n "$source_dsn" ]; then source_mode=external; else source_mode=review_stub; source_dsn="campus_analytics_source:$(required "$managed" CAMPUS_ACADEMIC_ANALYTICS_SOURCE_PASSWORD)@tcp(analytics-mysql:3306)/campus_academic_source?parseTime=true"; fi
cat >"$root/roles/analytics/generated.env" <<EOF
CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE=$analytics_current/bootstrap.yaml
CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR=$analytics_current/academic-rpc
CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_HOST_DIR=$analytics_current/analytics-redis
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE=$analytics_current/redis.conf
CAMPUS_ACADEMIC_DB_PASSWORD=$(required "$managed" CAMPUS_ACADEMIC_DB_PASSWORD)
CAMPUS_ACADEMIC_DB_ROOT_PASSWORD=$(required "$managed" CAMPUS_ACADEMIC_DB_ROOT_PASSWORD)
CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN=$source_dsn
CAMPUS_ACADEMIC_ANALYTICS_SOURCE_MODE=$source_mode
CAMPUS_ACADEMIC_ANALYTICS_SOURCE_PASSWORD=$(required "$managed" CAMPUS_ACADEMIC_ANALYTICS_SOURCE_PASSWORD)
CAMPUS_ACADEMIC_ANALYTICS_REDIS_USERNAME=default
CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD=$(required "$managed" CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD)
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE=ca.crt
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE=
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE=
CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME=analytics-redis.internal
CAMPUS_ACADEMIC_ANALYTICS_BIND_ADDRESS=0.0.0.0
EOF
find "$provider_files" "$analytics_files" -type d -exec chmod 0700 {} \;
find "$provider_files" "$analytics_files" -type f -exec chmod 0600 {} \;
chmod 0600 "$root/roles/provider/generated.env" "$root/roles/analytics/generated.env"
printf '%s\n' "academic bootstrap materials prepared: $root" >&2
