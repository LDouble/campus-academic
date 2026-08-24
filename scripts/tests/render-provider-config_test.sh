#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT HUP INT TERM

CAMPUS_PROVIDER_ACTIVE_PROVIDER=mock \
	"$repo_root/scripts/render-provider-config.sh" review "$root/mock.yaml"
grep -q '^  active_provider: mock$' "$root/mock.yaml"

if CAMPUS_PROVIDER_ACTIVE_PROVIDER=mock \
	"$repo_root/scripts/render-provider-config.sh" production "$root/invalid.yaml" >"$root/out" 2>&1; then
	printf '%s\n' 'production accepted mock Provider' >&2
	exit 1
fi

CAMPUS_PROVIDER_ACTIVE_PROVIDER=ouc \
	CAMPUS_ACADEMIC_PROVIDER_CONFIG_SOURCE="$repo_root/deploy/provider-ouc.json" \
	"$repo_root/scripts/render-provider-config.sh" production "$root/provider.yaml"
grep -q '^  active_provider: ouc$' "$root/provider.yaml"
grep -q '^  ouc: |$' "$root/provider.yaml"

