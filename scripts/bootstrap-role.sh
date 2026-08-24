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
bootstrap_config=$state_root/$environment/bootstrap.env
[ -r "$bootstrap_config" ] || { printf '%s\n' "缺少 bootstrap 配置: $bootstrap_config" >&2; exit 1; }
remote_base=$(awk -F= '$1 == "CAMPUS_REMOTE_DEPLOY_ROOT" { print substr($0, length($1) + 2); found++ } END { if (found != 1) exit 1 }' "$bootstrap_config")
case "$remote_base" in
/|''|*[!A-Za-z0-9_./-]*|*/../*|*/..)
	printf '%s\n' 'CAMPUS_REMOTE_DEPLOY_ROOT 必须是安全的绝对路径且不能包含 ..' >&2
	exit 1
	;;
/*) ;;
*) printf '%s\n' 'CAMPUS_REMOTE_DEPLOY_ROOT 必须是绝对路径' >&2; exit 1 ;;
esac
if [ "$role" = provider ]; then
	[ -s "$source_dir/provider-config.yaml" ] || {
		printf '%s\n' '缺少 Provider 配置，请先重新执行 campus-deploy setup' >&2
		exit 1
	}
fi
repo_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
mkdir -p "$work/files" "$work/assets/scripts" "$work/assets/deploy"
cp -R "$source_dir/." "$work/files/"
case "$role" in
provider)
	cp "$repo_root/scripts/deploy-provider.sh" "$work/assets/scripts/"
	cp "$repo_root/deploy/provider.compose.yaml" "$repo_root/deploy/provider.atrust.override.yaml" "$work/assets/deploy/"
	;;
analytics)
	cp "$repo_root/scripts/deploy-analytics.sh" "$work/assets/scripts/"
	cp "$repo_root/deploy/analytics.compose.yaml" "$work/assets/deploy/"
	;;
esac
printf 'CAMPUS_ENV=%s\nCAMPUS_ROLE=%s\n' "$environment" "$role" >"$work/bootstrap.env"
cp "$work/bootstrap.env" "$work/runtime.env"
printf 'environment=%s\nrole=%s\nschema=1\n' "$environment" "$role" >"$work/bundle.meta"
(
	cd "$work"
	find . -type f ! -name manifest.sha256 ! -name bundle.id -print | LC_ALL=C sort | while IFS= read -r file; do
		hash=$(openssl dgst -sha256 "$file" | awk '{print $NF}')
		printf '%s  %s\n' "$hash" "${file#./}"
	done
) >"$work/manifest.sha256"
bundle_id=$(openssl dgst -sha256 "$work/manifest.sha256" | awk '{print $NF}')
printf '%s\n' "$bundle_id" >"$work/bundle.id"
remote_root=$remote_base/bootstrap/$environment/$role
remote_release=$remote_root/releases/$bundle_id
academic_root=/opt/campus-academic-$environment
ssh -o BatchMode=yes "$target" "sudo install -d -m 0700 '$remote_release'"
COPYFILE_DISABLE=1 tar --no-xattrs -cf - -C "$work" . | ssh -o BatchMode=yes "$target" "set -eu
sudo tar -xf - -C '$remote_release'
sudo chmod -R go-rwx '$remote_release'
sudo sh -c 'set -eu
release=\$1
install_root=\$2
role_root=\$3
cd \"\$release\"
sha256sum -c manifest.sha256 >/dev/null
expected=\$(cat bundle.id)
actual=\$(sha256sum manifest.sha256 | awk \"{print \\\$1}\")
test \"\$expected\" = \"\$actual\"
install -d -m 0755 \"\$install_root/scripts\" \"\$install_root/deploy\"
for file in assets/scripts/*; do install -m 0755 \"\$file\" \"\$install_root/scripts/\"; done
for file in assets/deploy/*; do install -m 0644 \"\$file\" \"\$install_root/deploy/\"; done
ln -sfn \"\$release\" \"\$role_root/current.next\"
mv -Tf \"\$role_root/current.next\" \"\$role_root/current\"
' sh '$remote_release' '$academic_root' '$remote_root'"
printf '%s\n' "academic bootstrap installed: environment=$environment role=$role target=$target bundle=$bundle_id"
