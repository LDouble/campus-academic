#!/bin/sh

set -eu

environment=${1:-}
output=${2:-}
provider=${CAMPUS_PROVIDER_ACTIVE_PROVIDER:-ouc}
source_file=${CAMPUS_ACADEMIC_PROVIDER_CONFIG_SOURCE:-}

case "$environment" in review|production) ;; *)
	printf '%s\n' 'usage: render-provider-config.sh review|production OUTPUT' >&2
	exit 2
	;;
esac
[ -n "$output" ] || {
	printf '%s\n' 'usage: render-provider-config.sh review|production OUTPUT' >&2
	exit 2
}
case "$provider" in
mock)
	[ "$environment" = review ] || {
		printf '%s\n' 'Production 禁止使用 Mock Provider' >&2
		exit 1
	}
	;;
*[!A-Za-z0-9._-]*|'')
	printf '%s\n' 'CAMPUS_PROVIDER_ACTIVE_PROVIDER 格式错误' >&2
	exit 1
	;;
esac

umask 077
mkdir -p "$(dirname "$output")"
{
	printf '%s\n' 'academic_provider:'
	printf '  active_provider: %s\n' "$provider"
	printf '%s\n' '  mock_credentials: []'
	if [ "$provider" != mock ]; then
		[ -n "$source_file" ] || source_file=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)/deploy/provider-ouc.json
		[ -s "$source_file" ] || {
			printf '%s\n' "Provider 配置不存在: $source_file" >&2
			exit 1
		}
		printf '  %s: |\n' "$provider"
		sed 's/^/    /' "$source_file"
	fi
	printf '%s\n' 'security: {}'
} >"$output"
chmod 0600 "$output"

