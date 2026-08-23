#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

fake_state=$test_root/state
fake_log=$test_root/docker.log
mkdir -p "$fake_state"

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
CAMPUS_ACADEMIC_IMAGE=registry.example/academic@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
CAMPUS_ATRUST_IMAGE=registry.example/atrust@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CAMPUS_ATRUST_GATEWAY_HOME=$test_root/gateway-home
CAMPUS_ACADEMIC_BOOTSTRAP_HOST_FILE=$test_root/bootstrap.yaml
CAMPUS_ACADEMIC_PROVIDER_CONFIG_HOST_FILE=$test_root/provider-config.yaml
CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR=$test_root/rpc-tls
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR=$test_root/provider-redis-tls
CAMPUS_ATRUST_USERNAME_FILE=$test_root/atrust-username
CAMPUS_ATRUST_PASSWORD_FILE=$test_root/atrust-password
CAMPUS_ATRUST_CLIENT_NETWORK=test-atrust-clients
CAMPUS_ATRUST_EGRESS_NETWORK=test-atrust-egress
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

echo 'deploy-provider tests passed'
