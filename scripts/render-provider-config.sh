#!/bin/sh
set -eu
environment=${1:-}
output=${2:-}
provider=${CAMPUS_PROVIDER_ACTIVE_PROVIDER:-ouc}
source_file=${CAMPUS_ACADEMIC_PROVIDER_CONFIG_SOURCE:-}
case "$environment" in review|production) ;; *) printf '%s\n' 'usage: render-provider-config.sh review|production OUTPUT' >&2; exit 2 ;; esac
[ -n "$output" ] || { printf '%s\n' 'usage: render-provider-config.sh review|production OUTPUT' >&2; exit 2; }
case "$provider" in
mock) [ "$environment" = review ] || { printf '%s\n' 'Production 禁止使用 Mock Provider' >&2; exit 1; } ;;
ouc) [ -n "$source_file" ] || source_file=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)/deploy/provider-ouc.json ;;
*) printf '%s\n' '当前版本 CAMPUS_PROVIDER_ACTIVE_PROVIDER 仅支持 ouc；Review 额外支持 mock' >&2; exit 1 ;;
esac
if [ "$provider" != mock ]; then
	[ -s "$source_file" ] || { printf '%s\n' "Provider 配置不存在: $source_file" >&2; exit 1; }
fi
umask 077
directory=$(dirname "$output")
mkdir -p "$directory"
temporary=$(mktemp "$directory/.provider-config.XXXXXX")
trap 'rm -f "$temporary"' EXIT HUP INT TERM
{
	printf '%s\n' 'academic_provider:'
	printf '  active_provider: %s\n' "$provider"
	printf '%s\n' '  mock_credentials: []'
	if [ "$provider" = ouc ]; then
		printf '%s\n' '  ouc: |'
		sed 's/^/    /' "$source_file"
	fi
	printf '%s\n' 'security: {}'
} >"$temporary"
chmod 0600 "$temporary"
mv "$temporary" "$output"
trap - EXIT HUP INT TERM
