#!/bin/sh
set -eu
ROOT=$(CDPATH='' cd "${0%/*}/.." && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/herdr-plugin-recovery-test.XXXXXX")
trap 'status=$?; rm -rf "$WORK"; exit $status' EXIT INT TERM
awk '/^(validate_migration_entry|copy_migration_entry|restore_target_config|cleanup_plugin_build)\(\)/ { copying=1 } copying { print } copying && /^}/ { copying=0 }' "$ROOT/relay/plugin-build.sh" > "$WORK/functions.sh"
mkdir -p "$WORK/source/nested" "$WORK/target" "$WORK/backup"
printf retained > "$WORK/target/state"
printf original > "$WORK/backup/state"
ln -s "$WORK/backup/state" "$WORK/source/nested/link"
cat > "$WORK/check.sh" <<'EOF'
set -eu
. "$WORK/functions.sh"
TARGET_CONFIG_ROOT="$WORK/target"
if copy_migration_entry "$WORK/source" state; then
    echo 'nested symlink was accepted' >&2
    exit 1
fi
[ "$(cat "$TARGET_CONFIG_ROOT/state")" = retained ]
CONFIG_BACKUP="$WORK/backup"
target_config_existed=true
restore_target_config
[ "$(cat "$TARGET_CONFIG_ROOT/state")" = original ]
[ "$(cat "$CONFIG_BACKUP/state")" = original ]
failed=$(find "$WORK" -path '*/failed/state' -type f)
[ -n "$failed" ]
[ "$(cat "$failed")" = retained ]
EOF
WORK="$WORK" bash "$WORK/check.sh"
cat > "$WORK/failure.sh" <<'EOF'
set -eu
. "$WORK/functions.sh"
CONFIG_BACKUP="$WORK/backup"
SERVICE_BACKUP="$WORK/service-backup"
rollback_armed=true
rollback_plugin_migration() { return 1; }
trap cleanup_plugin_build EXIT
exit 1
EOF
printf service > "$WORK/service-backup"
if WORK="$WORK" bash "$WORK/failure.sh" > "$WORK/failure.log" 2>&1; then
    echo 'rollback failure became success' >&2
    exit 1
fi
[ -f "$WORK/backup/state" ]
[ -f "$WORK/service-backup" ]
grep -F 'recovery material retained' "$WORK/failure.log" >/dev/null
printf '%s\n' 'plugin recovery isolation tests passed'
