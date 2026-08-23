#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

fake_state=$test_root/state
fake_log=$test_root/docker.log
mkdir -p "$fake_state" "$test_root/rpc-tls" "$test_root/analytics-redis-tls"

cat >"$test_root/docker" <<'FAKE'
#!/bin/sh
set -eu
state=${FAKE_DOCKER_STATE:?}
log=${FAKE_DOCKER_LOG:?}
printf '%s\n' "$*" >>"$log"
if [ "$1" = compose ]; then
  shift
  command=
  service=
  for argument in "$@"; do
    case "$argument" in version|config|up|ps) command=$argument ;; analytics-mysql|analytics-redis|analytics-migrate|academic-analytics) service=$argument ;; esac
  done
  if [ "$command" = up ]; then
    for argument in "$@"; do
      case "$argument" in
        analytics-mysql|analytics-redis|academic-analytics)
          printf 'running' >"$state/$argument.status"
          printf 'healthy' >"$state/$argument.health"
          ;;
        analytics-migrate)
          printf 'exited' >"$state/$argument.status"
          printf '0' >"$state/$argument.exit"
          ;;
      esac
    done
  elif [ "$command" = ps ] && [ -n "$service" ]; then
    printf '%s-id\n' "$service"
  fi
  exit 0
fi
if [ "$1" = inspect ]; then
  format=$3
  container=$4
  service=${container%-id}
  case "$format" in
    *State.Status*) cat "$state/$service.status" ;;
    *State.Health*) cat "$state/$service.health" ;;
    *State.ExitCode*) cat "$state/$service.exit" ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 1
FAKE
chmod +x "$test_root/docker"

cat >"$test_root/bootstrap.yaml" <<'YAML'
environment: production
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
  server_name: academic-analytics
  redis:
    tls: true
    tls_files_root: /run/secrets/analytics-redis
    ca_file: redis-ca.crt
    client_cert_file: ""
    client_key_file: ""
    server_name: analytics-redis.internal
YAML
for file in ca.crt client.crt client.key server.crt server.key; do printf 'fixture\n' >"$test_root/rpc-tls/$file"; done
printf 'fixture\n' >"$test_root/analytics-redis-tls/redis-ca.crt"
printf 'port 6379\ntls-port 6379\ntls-cert-file /run/secrets/analytics-redis/server.crt\ntls-key-file /run/secrets/analytics-redis/server.key\ntls-ca-cert-file /run/secrets/analytics-redis/redis-ca.crt\n' >"$test_root/redis.conf"

cat >"$test_root/analytics.env" <<EOF
CAMPUS_ACADEMIC_ANALYTICS_IMAGE=registry.example/academic-analytics@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE=$test_root/bootstrap.yaml
CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR=$test_root/rpc-tls
CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_HOST_DIR=$test_root/analytics-redis-tls
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE=$test_root/redis.conf
CAMPUS_ACADEMIC_DB_PASSWORD=database-password
CAMPUS_ACADEMIC_DB_ROOT_PASSWORD=root-password
CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN=readonly-source
CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD=redis-password
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE=redis-ca.crt
CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME=analytics-redis.internal
EOF

run_deploy() {
  FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  ANALYTICS_ENV_FILE=$test_root/analytics.env ANALYTICS_DEPENDENCY_HEALTH_TIMEOUT=1 \
  ANALYTICS_DEPENDENCY_HEALTH_INTERVAL=1 "$repo_root/scripts/deploy-analytics.sh" production
}

output=$(run_deploy)
printf '%s' "$output" | grep -q 'production Analytics 发布完成'
grep -q 'up -d --no-build analytics-mysql analytics-redis' "$fake_log"
grep -q 'up --no-build --no-deps --abort-on-container-exit analytics-migrate' "$fake_log"
grep -q 'up -d --no-build --no-deps academic-analytics' "$fake_log"
if grep -Eq 'academic-provider|provider-redis|atrust' "$fake_log"; then
  echo 'Analytics 发布器不应启动 Provider、Provider Redis 或 aTrust' >&2
  exit 1
fi

sed 's/insecure: false/insecure: true/' "$test_root/bootstrap.yaml" >"$test_root/insecure-bootstrap.yaml"
sed "s#$test_root/bootstrap.yaml#$test_root/insecure-bootstrap.yaml#" "$test_root/analytics.env" >"$test_root/insecure.env"
if FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  ANALYTICS_ENV_FILE=$test_root/insecure.env "$repo_root/scripts/deploy-analytics.sh" production >"$test_root/insecure.out" 2>&1; then
  echo '明文 Analytics 未被拒绝' >&2
  exit 1
fi
grep -q 'Analytics 必须启用 mTLS' "$test_root/insecure.out"

sed 's#@sha256:[0-9a-f]*#:review#' "$test_root/analytics.env" >"$test_root/mutable.env"
if FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  ANALYTICS_ENV_FILE=$test_root/mutable.env "$repo_root/scripts/deploy-analytics.sh" production >"$test_root/mutable.out" 2>&1; then
  echo 'Production 可变 Analytics 镜像未被拒绝' >&2
  exit 1
fi
grep -q 'Production Analytics 镜像必须使用 64 位 sha256 摘要' "$test_root/mutable.out"

sed 's/environment: production/environment: review/' "$test_root/bootstrap.yaml" >"$test_root/review-bootstrap.yaml"
sed "s#${test_root}/bootstrap.yaml#${test_root}/review-bootstrap.yaml#" "$test_root/mutable.env" >"$test_root/review.env"
review_output=$(FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  ANALYTICS_ENV_FILE=$test_root/review.env ANALYTICS_DEPENDENCY_HEALTH_TIMEOUT=1 \
  ANALYTICS_DEPENDENCY_HEALTH_INTERVAL=1 "$repo_root/scripts/deploy-analytics.sh" review 2>&1)
printf '%s' "$review_output" | grep -q 'Review 警告：建议使用'
printf '%s' "$review_output" | grep -q 'review Analytics 发布完成'

echo 'deploy-analytics tests passed'
