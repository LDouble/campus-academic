#!/bin/sh
set -eu
environment=${1:-}
role=${2:-}
target=${3:-}
state_root=${CAMPUS_DEPLOY_STATE_DIR:-$HOME/.local/share/campus/deploy}
case "$environment" in review|production) ;; *) exit 2 ;; esac
case "$role" in provider|analytics) ;; *) exit 2 ;; esac
[ -n "$target" ] || exit 2
case "$state_root" in /*) ;; *) printf '%s\n' 'CAMPUS_DEPLOY_STATE_DIR 必须是绝对路径' >&2; exit 1 ;; esac
source_dir=$state_root/$environment/roles/$role/files
[ -s "$source_dir/bootstrap.yaml" ] || { printf '%s\n' "缺少 $role bootstrap.yaml: $source_dir" >&2; exit 1; }
if [ "$role" = provider ] && [ ! -s "$source_dir/provider-config.yaml" ]; then
	"$(dirname "$0")/render-provider-config.sh" "$environment" "$source_dir/provider-config.yaml"
fi
repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/files" "$work/deploy"
cp -R "$source_dir/." "$work/files/"
cp "$repo_root/scripts/deploy-$role.sh" "$work/deploy/"
cp "$repo_root/deploy/$role.compose.yaml" "$work/deploy/"
[ "$role" != provider ] || cp "$repo_root/deploy/provider.atrust.override.yaml" "$work/deploy/"
(
	cd "$work"
	find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do
		hash=$(openssl dgst -sha256 "$file" | awk '{print $NF}')
		printf '%s  %s\n' "$hash" "$file"
	done
) >"$work/manifest.sha256"
bundle_id=$(openssl dgst -sha256 "$work/manifest.sha256" | awk '{print $NF}')
remote_root=/opt/campus-academic-$environment/bootstrap/$role
remote_release=$remote_root/releases/$bundle_id
ssh -o BatchMode=yes "$target" "sudo install -d -m 0700 '$remote_release'"
COPYFILE_DISABLE=1 tar --no-xattrs -cf - -C "$work" . | ssh -o BatchMode=yes "$target" "sudo tar -xf - -C '$remote_release' && sudo ln -sfn '$remote_release' '$remote_root/current'"
printf '%s\n' "academic bootstrap installed: environment=$environment role=$role target=$target bundle=$bundle_id"
