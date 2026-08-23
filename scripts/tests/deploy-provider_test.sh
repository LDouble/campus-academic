#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

fake_state=$test_root/state
fake_log=$test_root/docker.log
mkdir -p "$fake_state"

grep -Fq '${CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT:-9090}:9090' "$repo_root/deploy/compose.yaml" || {
  echo 'Provider Compose 缺少可配置发布端口' >&2
  exit 1
}
grep -Fq '${CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT:-9090}:9090' "$repo_root/deploy/provider.compose.yaml" || {
  echo 'Provider 独立 Compose 缺少可配置发布端口' >&2
  exit 1
}
if grep -Eq 'academic-analytics|analytics-mysql|analytics-redis|^[[:space:]]+provider-redis:' "$repo_root/deploy/provider.compose.yaml"; then
  echo 'Provider 独立 Compose 混入了非 Provider 服务或本机 Redis' >&2
  exit 1
fi
grep -Fq '${CAMPUS_ACADEMIC_ANALYTICS_PUBLISHED_PORT:-9091}:9091' "$repo_root/deploy/compose.yaml" || {
  echo 'Analytics Compose 缺少可配置发布端口' >&2
  exit 1
}

cat >"$test_root/docker" <<'FAKE'
#!/bin/sh
set -eu
state=${FAKE_DOCKER_STATE:?}
log=${FAKE_DOCKER_LOG:?}
printf 'COMPOSE_PROJECT_NAME=%s CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT=%s %s\n' \
  "${COMPOSE_PROJECT_NAME:-}" "${CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT:-}" "$*" >>"$log"

if [ "$1" = compose ]; then
  shift
  command=
  service=
  for argument in "$@"; do
    case "$argument" in
      version|config|up|ps) command=$argument ;;
      atrust-gateway|academic-provider) service=$argument ;;
    esac
  done
  case "$command:$service" in
    up:atrust-gateway)
      : >"$state/atrust.exists"
      printf 'running' >"$state/atrust.status"
      printf 'healthy' >"$state/atrust.health"
      ;;
    up:academic-provider)
      : >"$state/provider.exists"
      printf 'running' >"$state/provider.status"
      printf 'healthy' >"$state/provider.health"
      ;;
    ps:atrust-gateway) printf '%s\n' atrust-id ;;
    ps:academic-provider) printf '%s\n' provider-id ;;
  esac
  exit 0
fi

if [ "$1" = network ] && [ "$2" = inspect ]; then
  if [ "$3" = --format ]; then
    network=$5
    cat "$state/network.$network.internal"
  else
    network=$3
    test -f "$state/network.$network.internal"
  fi
  exit $?
fi

if [ "$1" = network ] && [ "$2" = create ]; then
  internal=false
  network=
  for argument in "$@"; do
    [ "$argument" = --internal ] && internal=true
    network=$argument
  done
  printf '%s' "$internal" >"$state/network.$network.internal"
  printf '%s\n' "$network"
  exit 0
fi

if [ "$1" = ps ]; then
  test -f "$state/atrust.exists" && printf '%s\n' atrust-id
  exit 0
fi

if [ "$1" = inspect ]; then
  format=$3
  container=$4
  case "$container:$format" in
    atrust-id:*State.Status*) cat "$state/atrust.status" ;;
    atrust-id:*State.Health*) cat "$state/atrust.health" ;;
    provider-id:*State.Status*) cat "$state/provider.status" ;;
    provider-id:*State.Health*) cat "$state/provider.health" ;;
    *) exit 1 ;;
  esac
  exit 0
fi

if [ "$1" = restart ] || [ "$1" = start ]; then
  printf 'running' >"$state/atrust.status"
  printf 'healthy' >"$state/atrust.health"
  exit 0
fi

exit 1
FAKE
chmod +x "$test_root/docker"

mkdir -p "$test_root/gateway-home/scripts"
cat >"$test_root/gateway-home/scripts/ensure-gateway.sh" <<'EOF'
#!/bin/sh
set -eu
printf '%s|%s\n' "$1" "$ATRUST_ENV_FILE" >"$TEST_GATEWAY_LOG"
echo 'independent gateway ensured'
EOF
chmod +x "$test_root/gateway-home/scripts/ensure-gateway.sh"

cat >"$test_root/bootstrap.yaml" <<'YAML'
environment: production
provider_config_file: /etc/campus-academic/provider-config.yaml
provider:
  listen_address: ":9090"
  target: 127.0.0.1:9090
  insecure: false
  http_proxy_url: http://atrust-gateway:8888
  tls_files_root: /run/secrets/academic-rpc
  ca_file: ca.crt
  client_cert_file: client.crt
  client_key_file: client.key
  server_cert_file: server.crt
  server_key_file: server.key
  server_name: academic-provider
YAML

cat >"$test_root/provider-config.yaml" <<'YAML'
academic_provider:
  active_provider: ouc
  ouc: '{}'
YAML

printf 'username\n' >"$test_root/atrust-username"
printf 'password\n' >"$test_root/atrust-password"
mkdir -p "$test_root/rpc-tls" "$test_root/provider-redis-tls"
for file in ca.crt client.crt client.key server.crt server.key; do
  printf 'fixture\n' >"$test_root/rpc-tls/$file"
done

cat >"$test_root/provider.env" <<EOF
CAMPUS_ACADEMIC_PROVIDER_IMAGE=registry.example/academic-provider@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
CAMPUS_ACADEMIC_ANALYTICS_IMAGE=registry.example/academic-analytics@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
CAMPUS_ATRUST_IMAGE=registry.example/atrust@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CAMPUS_ATRUST_GATEWAY_HOME=$test_root/gateway-home
CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE=$test_root/bootstrap.yaml
CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE=$test_root/provider-config.yaml
CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR=$test_root/rpc-tls
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR=$test_root/provider-redis-tls
CAMPUS_ATRUST_USERNAME_FILE=$test_root/atrust-username
CAMPUS_ATRUST_PASSWORD_FILE=$test_root/atrust-password
CAMPUS_ATRUST_CLIENT_NETWORK=campus-production-atrust-clients
CAMPUS_ATRUST_EGRESS_NETWORK=campus-production-atrust-egress
EOF

run_deploy() {
  FAKE_DOCKER_STATE=$fake_state \
  FAKE_DOCKER_LOG=$fake_log \
  DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/provider.env \
  PROVIDER_DEPENDENCY_HEALTH_TIMEOUT=1 \
  PROVIDER_DEPENDENCY_HEALTH_INTERVAL=1 \
  TEST_GATEWAY_LOG=$test_root/gateway.log \
  "$repo_root/scripts/deploy-provider.sh" production
}

first_output=$(run_deploy)
printf '%s' "$first_output" | grep -q '调用独立 campus-atrust-gateway 依赖发布器'
printf '%s' "$first_output" | grep -q 'independent gateway ensured'
printf '%s' "$first_output" | grep -q 'production Provider 发布完成'
grep -q "^production|$test_root/provider.env$" "$test_root/gateway.log"
grep -q 'COMPOSE_PROJECT_NAME=campus-academic-production-provider .*academic-provider' "$fake_log"

sed 's/active_provider: ouc/active_provider: mock/' "$test_root/provider-config.yaml" >"$test_root/provider-config.mock.yaml"
sed "s#provider-config.yaml#provider-config.mock.yaml#" "$test_root/provider.env" >"$test_root/provider.mock.env"
if FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/provider.mock.env \
  "$repo_root/scripts/deploy-provider.sh" production >"$test_root/mock.out" 2>&1; then
  echo 'Production Mock 配置未被拒绝' >&2
  exit 1
fi
grep -q 'Production 禁止使用 Mock Provider' "$test_root/mock.out"

sed "s#CAMPUS_ATRUST_GATEWAY_HOME=.*#CAMPUS_ATRUST_GATEWAY_HOME=$test_root/missing-gateway#" \
  "$test_root/provider.env" >"$test_root/provider.missing-gateway.env"
if FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/provider.missing-gateway.env \
  "$repo_root/scripts/deploy-provider.sh" production >"$test_root/missing.out" 2>&1; then
  echo '缺失的独立 aTrust 仓库未被拒绝' >&2
  exit 1
fi
grep -q 'scripts/ensure-gateway.sh' "$test_root/missing.out"

if COMPOSE_PROJECT_NAME=campus-academic-production-analytics \
  FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/provider.env \
  "$repo_root/scripts/deploy-provider.sh" production >"$test_root/wrong-project.out" 2>&1; then
  echo 'Provider 错误角色 Compose project 未被拒绝' >&2
  exit 1
fi
grep -q 'COMPOSE_PROJECT_NAME 必须是 campus-academic-production-provider' "$test_root/wrong-project.out"

if CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT=0 \
  FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/provider.env \
  "$repo_root/scripts/deploy-provider.sh" production >"$test_root/wrong-port.out" 2>&1; then
  echo 'Provider 非法发布端口未被拒绝' >&2
  exit 1
fi
grep -q 'CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT 必须是 1 到 65535' "$test_root/wrong-port.out"

sed 's/campus-production-atrust-clients/campus-atrust-clients/' \
  "$test_root/provider.env" >"$test_root/legacy-network.env"
if FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/legacy-network.env \
  "$repo_root/scripts/deploy-provider.sh" production >"$test_root/legacy-network.out" 2>&1; then
  echo 'Provider 旧式共享 aTrust 网络未被拒绝' >&2
  exit 1
fi
grep -q 'CAMPUS_ATRUST_CLIENT_NETWORK 必须带有 campus-production-atrust-clients' "$test_root/legacy-network.out"

sed 's/environment: production/environment: review/' "$test_root/bootstrap.yaml" >"$test_root/review-bootstrap.yaml"
sed "s#${test_root}/bootstrap.yaml#${test_root}/review-bootstrap.yaml#; s/campus-production-atrust-clients/campus-review-atrust-clients/; s/campus-production-atrust-egress/campus-review-atrust-egress/" \
  "$test_root/provider.env" >"$test_root/review.env"
review_output=$(COMPOSE_PROJECT_NAME=campus-academic-review-provider \
  CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT=19090 \
  FAKE_DOCKER_STATE=$fake_state FAKE_DOCKER_LOG=$fake_log DOCKER_BIN=$test_root/docker \
  PROVIDER_ENV_FILE=$test_root/review.env PROVIDER_DEPENDENCY_HEALTH_TIMEOUT=1 \
  PROVIDER_DEPENDENCY_HEALTH_INTERVAL=1 TEST_GATEWAY_LOG=$test_root/gateway.log \
  "$repo_root/scripts/deploy-provider.sh" review)
printf '%s' "$review_output" | grep -q 'review Provider 发布完成'
grep -q 'COMPOSE_PROJECT_NAME=campus-academic-review-provider CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT=19090' "$fake_log"

echo 'deploy-provider tests passed'
