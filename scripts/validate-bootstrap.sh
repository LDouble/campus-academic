#!/bin/sh
set -eu
environment=${1:-}
state_root=${CAMPUS_DEPLOY_STATE_DIR:-$HOME/.local/share/campus/deploy}
case "$environment" in review|production) ;; *) exit 2 ;; esac
for role in provider analytics; do
	files=$state_root/$environment/roles/$role/files
	[ -s "$files/bootstrap.yaml" ] || { printf '%s\n' "缺少 $role bootstrap.yaml" >&2; exit 1; }
	[ -s "$files/academic-rpc/ca.crt" ] || { printf '%s\n' "缺少 $role RPC CA" >&2; exit 1; }
	[ -s "$files/academic-rpc/server.crt" ] || { printf '%s\n' "缺少 $role RPC server certificate" >&2; exit 1; }
	[ -s "$files/academic-rpc/server.key" ] || { printf '%s\n' "缺少 $role RPC server key" >&2; exit 1; }
done
[ -s "$state_root/$environment/roles/provider/files/provider-config.yaml" ] || { printf '%s\n' '缺少 Provider 配置' >&2; exit 1; }
[ -s "$state_root/$environment/roles/analytics/files/redis.conf" ] || { printf '%s\n' '缺少 Analytics Redis 配置' >&2; exit 1; }
printf '%s\n' "academic bootstrap configuration is valid: $state_root/$environment"

