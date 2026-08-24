#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT HUP INT TERM
mkdir -p "$root/review/roles/provider/files"
printf '%s\n' 'environment: review' >"$root/review/roles/provider/files/bootstrap.yaml"

for invalid_root in relative / /opt/campus/../escape '/opt/campus bad'; do
	printf 'CAMPUS_REMOTE_DEPLOY_ROOT=%s\n' "$invalid_root" >"$root/review/bootstrap.env"
	if CAMPUS_DEPLOY_STATE_DIR="$root" "$repo_root/scripts/bootstrap-role.sh" \
		review provider root@example.invalid >"$root/out" 2>&1; then
		printf '%s\n' "invalid remote root accepted: $invalid_root" >&2
		exit 1
	fi
done

printf '%s\n' 'bootstrap-role path tests passed'
