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

grep -q "sudo sh -c 'set -eu" "$repo_root/scripts/bootstrap-role.sh"
if grep -q "^[[:space:]]*cd '\$remote_release'" "$repo_root/scripts/bootstrap-role.sh"; then
	printf '%s\n' 'remote release is entered outside the privileged shell' >&2
	exit 1
fi
grep -q '请先重新执行 campus-deploy setup' "$repo_root/scripts/bootstrap-role.sh"

printf '%s\n' 'bootstrap-role path tests passed'
