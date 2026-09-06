#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-credential-hygiene.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

# shellcheck source=/dev/null
. "$REPO_DIR/relay/common.sh"

ENV_FILE="$WORK_DIR/config/relay.env"
mkdir -p "$(dirname "$ENV_FILE")"
export GH_TOKEN=SENTINEL_GH_TOKEN_DO_NOT_PERSIST
export GITHUB_TOKEN=SENTINEL_GITHUB_TOKEN_DO_NOT_PERSIST
export HERDR_GITHUB_TOKEN_FILE="$WORK_DIR/SENTINEL_GITHUB_TOKEN_POINTER"

if ensure_relay_env "$ENV_FILE" >"$WORK_DIR/setup.log" 2>&1; then
    echo "relay setup accepted ambient GitHub credentials" >&2
    exit 1
fi
if grep -R -E 'SENTINEL_(GH|GITHUB)' "$WORK_DIR/config" "$WORK_DIR/setup.log" 2>/dev/null; then
    echo "relay setup persisted or logged an ambient GitHub credential sentinel" >&2
    exit 1
fi

unset GH_TOKEN GITHUB_TOKEN HERDR_GITHUB_TOKEN_FILE
ensure_relay_env "$ENV_FILE" >/dev/null
if grep -E '^(GH_TOKEN|GITHUB_TOKEN|HERDR_GITHUB_TOKEN_FILE)=' "$ENV_FILE"; then
    echo "relay environment contains a GitHub credential or pointer" >&2
    exit 1
fi
if [ -e "$WORK_DIR/config/github-token" ]; then
    echo "relay setup created a persistent GitHub token file" >&2
    exit 1
fi

printf 'legacy-token\n' > "$WORK_DIR/config/github-token"
ensure_relay_env "$ENV_FILE" >/dev/null
test ! -e "$WORK_DIR/config/github-token"

printf 'legacy-target\n' > "$WORK_DIR/legacy-token-target"
ln -s "$WORK_DIR/legacy-token-target" "$WORK_DIR/config/github-token"
ensure_relay_env "$ENV_FILE" >/dev/null
test ! -e "$WORK_DIR/config/github-token"
test "$(cat "$WORK_DIR/legacy-token-target")" = legacy-target

ensure_relay_env "$ENV_FILE" >/dev/null
test ! -e "$WORK_DIR/config/github-token"

echo "credential hygiene shell tests passed"
