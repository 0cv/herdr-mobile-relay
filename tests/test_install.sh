#!/bin/sh
set -eu

REPO_DIR=$(CDPATH='' cd "${0%/*}/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-install-test.XXXXXX")
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

# Load the installer functions without executing main.
sed '$d' "$REPO_DIR/install.sh" > "$WORK_DIR/install-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/install-functions.sh"

release_json='{
  "assets": [{
    "url": "https://api.github.com/repos/0cv/herdr-mobile-relay-dev/releases/assets/123",
    "name": "herdr-mobile-relay_0.9.0_linux_amd64.tar.gz",
    "uploader": {
      "url": "https://api.github.com/users/0cv"
    }
  }, {
    "url": "https://api.github.com/repos/0cv/herdr-mobile-relay-dev/releases/assets/124",
    "name": "checksums.txt",
    "uploader": {
      "url": "https://api.github.com/users/0cv"
    }
  }]
}'

archive_url=$(resolve_asset_url "$release_json" "herdr-mobile-relay_0.9.0_linux_amd64.tar.gz")
checksum_url=$(resolve_asset_url "$release_json" "checksums.txt")
test "$archive_url" = "https://api.github.com/repos/0cv/herdr-mobile-relay-dev/releases/assets/123"
test "$checksum_url" = "https://api.github.com/repos/0cv/herdr-mobile-relay-dev/releases/assets/124"

commit_json='{"sha":"0123456789abcdef0123456789abcdef01234567","commit":{"tree":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}'
test "$(resolve_tag_revision "$commit_json")" = "0123456789abcdef0123456789abcdef01234567"

sentinel_root="$WORK_DIR/custom-root"
write_install_sentinel "$sentinel_root"
canonical_root=$(CDPATH='' cd "$sentinel_root" && pwd -P)
grep -Fx 'product=herdr-mobile-relay' "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null
grep -Fx "root=$canonical_root" "$sentinel_root/.herdr-mobile-relay-installation" >/dev/null

unowned_root="$WORK_DIR/unowned"
mkdir -p "$unowned_root"
printf 'personal data\n' > "$unowned_root/keep.txt"
if (write_install_sentinel "$unowned_root") 2>/dev/null; then
    echo "write_install_sentinel claimed a nonempty unowned directory" >&2
    exit 1
fi
[ -f "$unowned_root/keep.txt" ]
[ ! -e "$unowned_root/.herdr-mobile-relay-installation" ]

legacy_home="$WORK_DIR/legacy-home"
legacy_release="$legacy_home/.local/share/herdr-mobile-relay"
legacy_config="$legacy_home/.config/herdr-mobile-relay"
legacy_cache="$legacy_home/.cache/herdr-mobile-relay"
mkdir -p "$legacy_config/push" "$legacy_cache/claude-history" "$legacy_cache/uploads"
printf "HERDR_RELAY_TOKEN='legacy-token'\nHERDR_RELAY_INSTANCE_ID='legacy-instance'\n" > "$legacy_config/relay.env"
printf '[]\n' > "$legacy_config/push/subscriptions.json"
printf '{"id":"legacy"}\n' > "$legacy_cache/activity.jsonl"
printf '{"history":["preserved"]}\n' > "$legacy_cache/claude-history/pane.json"
HOME="$legacy_home"
prepare_install_roots "$legacy_release" "$legacy_config" "$legacy_cache"
test -f "$legacy_config/.herdr-mobile-relay-installation"
test -f "$legacy_cache/.herdr-mobile-relay-installation"
grep -F legacy-token "$legacy_config/relay.env" >/dev/null
grep -F preserved "$legacy_cache/claude-history/pane.json" >/dev/null

xdg_home="$WORK_DIR/xdg-home"
xdg_release="$xdg_home/.local/share/herdr-mobile-relay"
xdg_config="$xdg_home/.config/herdr-mobile-relay"
old_cache="$xdg_home/.cache/herdr-mobile-relay"
new_cache="$xdg_home/custom-cache/herdr-mobile-relay"
mkdir -p "$xdg_config/push" "$old_cache/claude-history"
printf "HERDR_RELAY_TOKEN='xdg-token'\n" > "$xdg_config/relay.env"
printf '[]\n' > "$xdg_config/push/subscriptions.json"
printf '{"history":["migrated"]}\n' > "$old_cache/claude-history/pane.json"
mkdir -p "$new_cache"
printf '#!/bin/sh\n' > "$new_cache/post-install.sh"
printf 'legacy waiter\n' > "$new_cache/post-install.log"
HOME="$xdg_home"
prepare_install_roots "$xdg_release" "$xdg_config" "$new_cache"
test ! -e "$old_cache"
grep -F migrated "$new_cache/claude-history/pane.json" >/dev/null
grep -F 'legacy waiter' "$new_cache/post-install.log" >/dev/null
test -f "$new_cache/.herdr-mobile-relay-installation"

echo "install shell tests passed"
