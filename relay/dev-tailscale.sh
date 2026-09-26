#!/bin/bash
# Explicit, isolated development entrypoint for the managed foreground owner.
# It never installs, logs in, discovers a personal profile, or runs the CLI
# until the operator has selected a private root, binaries and ports.
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd -P)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd -P)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

usage() {
    echo "Managed Tailscale development requires a supported authenticated v1.102.4 Unix daemon." >&2
    echo "MacSys GUI and App Store Tailscale are not supported by this LocalAPI adapter." >&2
    echo "Interactive: make dev-tailscale (select an existing private root, exact binaries/socket, and ports)." >&2
    echo "Scripted: HERDR_DEV_TAILSCALE_ENABLE=1 HERDR_DEV_TAILSCALE_DIR=/private/path \\" >&2
    echo "  HERDR_DEV_TAILSCALE_BIN=/path/to/tailscale HERDR_DEV_HERDR_BIN=/path/to/herdr \\" >&2
    echo "  HERDR_DEV_HERDR_SOCKET=/path/to/herdr.sock HERDR_DEV_TAILSCALE_PORT=18377 \\" >&2
    echo "  HERDR_DEV_TAILSCALE_PLUGIN_PORT=18378 HERDR_DEV_TAILSCALE_HTTPS_PORT=8443 make dev-tailscale" >&2
    echo "Create the private root first with mkdir -m 700 /private/path. No daemon is started or logged in." >&2
}

prompt_missing() {
    local variable="$1" label="$2" default="${3:-}" entered=""
    [ -n "${!variable:-}" ] && return 0
    [ -t 0 ] || { usage; exit 2; }
    if [ -n "$default" ]; then
        read -r -p "$label [$default]: " entered || { echo "Cancelled; nothing was started." >&2; exit 2; }
        entered="${entered:-$default}"
    else
        read -r -p "$label: " entered || { echo "Cancelled; nothing was started." >&2; exit 2; }
        [ -n "$entered" ] || { echo "✗ $label is required; nothing was started." >&2; exit 2; }
    fi
    printf -v "$variable" '%s' "$entered"
    export "${variable?}"
}

case "${HERDR_DEV_TAILSCALE_ENABLE:-}" in
    1) ;;
    '')
        [ -t 0 ] || { usage; exit 2; }
        echo "Managed Tailscale Serve is NOT a Cloudflare tunnel: it changes one tailnet HTTPS route for this foreground pane."
        echo "No Tailscale CLI, daemon, LocalAPI or Herdr socket is contacted before you explicitly select them."
        read -r -p "Continue with a supported, already authenticated Unix daemon? [y/N] " consent || exit 2
        case "$consent" in
            y|Y|yes|YES) export HERDR_DEV_TAILSCALE_ENABLE=1 ;;
            *) echo "Cancelled; nothing was started."; exit 2 ;;
        esac
        ;;
    *) echo "✗ HERDR_DEV_TAILSCALE_ENABLE must be 1 to opt in." >&2; exit 2 ;;
esac

prompt_missing HERDR_DEV_TAILSCALE_DIR "Existing absolute private state directory (mkdir -m 700 PATH first)"
prompt_missing HERDR_DEV_TAILSCALE_BIN "Absolute path to supported Tailscale CLI"
prompt_missing HERDR_DEV_HERDR_BIN "Absolute path to Herdr executable"
prompt_missing HERDR_DEV_HERDR_SOCKET "Absolute path to running Herdr Unix socket"
prompt_missing HERDR_DEV_TAILSCALE_PORT "Development relay TCP port" 18377
prompt_missing HERDR_DEV_TAILSCALE_PLUGIN_PORT "Development plugin UDP port" 18378
prompt_missing HERDR_DEV_TAILSCALE_HTTPS_PORT "Tailscale HTTPS Serve port" 8443
case "${HERDR_DEV_TAILSCALE_DIR:-}" in
    /*) ;;
    *) usage; echo "✗ Choose an existing absolute private development root." >&2; exit 2 ;;
esac
[ -d "$HERDR_DEV_TAILSCALE_DIR" ] && [ ! -L "$HERDR_DEV_TAILSCALE_DIR" ] || {
    echo "✗ Create a private development root first (mkdir -m 700 PATH)." >&2
    exit 2
}
DEV_ROOT="$(cd "$HERDR_DEV_TAILSCALE_DIR" && pwd -P)"
[ "$DEV_ROOT" != / ] && [ "$DEV_ROOT" != "$HOME" ] || { echo "✗ Root or home cannot be a dev state directory." >&2; exit 2; }
case "$(uname -s)" in
    Darwin) ROOT_MODE="$(stat -f '%Lp' "$DEV_ROOT")" ;;
    Linux) ROOT_MODE="$(stat -c '%a' "$DEV_ROOT")" ;;
    *) echo "✗ Only Linux and macOS development are supported." >&2; exit 2 ;;
esac
[ "$ROOT_MODE" = 700 ] || { echo "✗ Development state root must have mode 0700." >&2; exit 2; }
for protected in "${XDG_CONFIG_HOME:-$HOME/.config}/herdr-mobile-relay" \
    "${HERDR_PLUGIN_CONFIG_DIR:-$HOME/.config/herdr-mobile-relay}" \
    "${XDG_DATA_HOME:-$HOME/.local/share}/herdr-mobile-relay"; do
    [ -d "$protected" ] || continue
    canonical_protected="$(cd "$protected" && pwd -P)"
    case "$DEV_ROOT/" in
        "$canonical_protected/"* ) echo "✗ Development root overlaps installed relay state." >&2; exit 2 ;;
    esac
    case "$canonical_protected/" in
        "$DEV_ROOT/"* ) echo "✗ Installed relay state overlaps development root." >&2; exit 2 ;;
    esac
done
case "$DEV_ROOT/" in
    "$REPO_DIR/relay/.dev/"*) echo "✗ Do not reuse dev-tunnel state." >&2; exit 2 ;;
esac
[ -z "${HERDR_RELAY_ENV:-}" ] || [ "$HERDR_RELAY_ENV" = "$DEV_ROOT/relay.env" ] || {
    echo "✗ Inherited relay state path conflicts with the selected development root." >&2
    exit 2
}
for binary in "${HERDR_DEV_TAILSCALE_BIN:-}" "${HERDR_DEV_HERDR_BIN:-}"; do
    case "$binary" in /*) [ -x "$binary" ] && [ ! -d "$binary" ] || { usage; exit 2; } ;; *) usage; exit 2 ;; esac
done
case "${HERDR_DEV_HERDR_SOCKET:-}" in /*) ;; *) usage; exit 2 ;; esac
for port in "${HERDR_DEV_TAILSCALE_PORT:-}" "${HERDR_DEV_TAILSCALE_PLUGIN_PORT:-}" "${HERDR_DEV_TAILSCALE_HTTPS_PORT:-}"; do
    case "$port" in ''|*[!0-9]*) usage; exit 2 ;; esac
    [ "$port" -ge 1024 ] && [ "$port" -le 65535 ] || { usage; exit 2; }
    case "$port" in 8375|8376|18375|18376) echo "✗ Production and dev-tunnel ports are reserved." >&2; exit 2 ;; esac
done
[ "$HERDR_DEV_TAILSCALE_PORT" != "$HERDR_DEV_TAILSCALE_PLUGIN_PORT" ] &&
    [ "$HERDR_DEV_TAILSCALE_PORT" != "$HERDR_DEV_TAILSCALE_HTTPS_PORT" ] &&
    [ "$HERDR_DEV_TAILSCALE_PLUGIN_PORT" != "$HERDR_DEV_TAILSCALE_HTTPS_PORT" ] || {
    echo "✗ Choose three distinct development ports." >&2
    exit 2
}
case "${1:-}" in
    '') [ "$#" -eq 0 ] || { usage; exit 2; } ;;
    --confirm-serve) [ "$#" -eq 1 ] || { usage; exit 2; } ;;
    *) usage; exit 2 ;;
esac

ENV_FILE="$DEV_ROOT/relay.env"
MARKER="$DEV_ROOT/.herdr-dev-tailscale"
for leaf in relay.env .herdr-dev-tailscale bin bin/herdr-mobile-relay home config cache data web; do
    [ ! -L "$DEV_ROOT/$leaf" ] || { echo "✗ Symlinks inside development state are refused." >&2; exit 2; }
done
if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
    [ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] && [ -f "$MARKER" ] && [ ! -L "$MARKER" ] &&
        [ "$(wc -l < "$MARKER")" -eq 1 ] && grep -Fxq 'HERDR_DEV_TAILSCALE_ROOT=1' "$MARKER" || {
        echo "✗ Existing state is not a marked private development relay; no bytes were changed." >&2
        exit 1
    }
    for pair in "HERDR_RELAY_PORT:$HERDR_DEV_TAILSCALE_PORT" \
        "HERDR_RELAY_PLUGIN_PORT:$HERDR_DEV_TAILSCALE_PLUGIN_PORT"; do
        key="${pair%%:*}"; expected="${pair#*:}"
        [ "$(env_file_value "$ENV_FILE" "$key")" = "$expected" ] || {
            echo "✗ Development ports changed; enrolled state was left untouched." >&2
            exit 1
        }
    done
    [ "$(env_file_value "$ENV_FILE" HERDR_RELAY_HOST)" = 127.0.0.1 ] || {
        echo "✗ Development backend host changed; state was left untouched." >&2
        exit 1
    }
    case "$(env_file_value "$ENV_FILE" HERDR_RELAY_REARM_BOOTSTRAP | tr '[:upper:]' '[:lower:]')" in
        1|true|yes|on) echo "✗ Refusing to reset enrolled development devices." >&2; exit 1 ;;
    esac
else
    [ ! -e "$MARKER" ] && [ ! -L "$MARKER" ] || { echo "✗ Stale development marker needs inspection." >&2; exit 1; }
    token="$(generate_token)"; instance="$(generate_instance_id)"
    [ "${#token}" -eq 32 ] && [ -n "$instance" ] || { echo "✗ Cannot prepare private development credentials." >&2; exit 1; }
    printf 'HERDR_DEV_TAILSCALE_ROOT=1\n' > "$MARKER"
    chmod 600 "$MARKER"
    {
        printf 'HERDR_RELAY_TOKEN=%s\n' "$token"
        printf 'HERDR_RELAY_INSTANCE_ID=%s\n' "$instance"
        printf 'HERDR_RELAY_HOST=127.0.0.1\n'
        printf 'HERDR_RELAY_PORT=%s\n' "$HERDR_DEV_TAILSCALE_PORT"
        printf 'HERDR_RELAY_PLUGIN_PORT=%s\n' "$HERDR_DEV_TAILSCALE_PLUGIN_PORT"
        printf 'HERDR_RELAY_REARM_BOOTSTRAP=0\n'
    } > "$ENV_FILE"
    chmod 600 "$ENV_FILE"
fi

command -v bun >/dev/null 2>&1 && command -v go >/dev/null 2>&1 || {
    echo "✗ Building the isolated development relay requires Bun 1.4 and Go 1.27.1." >&2
    exit 1
}
version="$(sed -n 's/^version = "\([0-9.]*\)"$/\1/p' "$REPO_DIR/herdr-plugin.toml")"
revision="$(git -C "$REPO_DIR" rev-parse HEAD)"
[ -n "$version" ] && [ "${#revision}" -eq 40 ] || { echo "✗ Cannot determine coherent development build identity." >&2; exit 1; }
mkdir -p "$DEV_ROOT/bin" "$DEV_ROOT/home" "$DEV_ROOT/config" "$DEV_ROOT/cache" "$DEV_ROOT/data"
chmod 700 "$DEV_ROOT/bin" "$DEV_ROOT/home" "$DEV_ROOT/config" "$DEV_ROOT/cache" "$DEV_ROOT/data"
BUILD_DIR="$(mktemp -d "$DEV_ROOT/.build.XXXXXX")"
bun run --cwd "$REPO_DIR/frontend" build
cp -R "$REPO_DIR/frontend/dist" "$BUILD_DIR/web"
bun "$REPO_DIR/scripts/stamp-web-version.mjs" "$BUILD_DIR/web/version.json" "$version" "$revision"
bun "$REPO_DIR/frontend/scripts/validate-build.mjs" "$BUILD_DIR/web"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version -X main.revision=$revision" \
    -o "$BUILD_DIR/herdr-mobile-relay" "$REPO_DIR/cmd/herdr-mobile-relay"
# Do not replace a running dev binary or web root: the managed launcher must
# first prove no private session/control/owner record exists for this root.
[ ! -e "$DEV_ROOT/tailscale-session.env" ] && [ ! -e "$DEV_ROOT/tailscale-control.sock" ] &&
    [ ! -e "$DEV_ROOT/owner.lock" ] || {
    echo "✗ Prior managed development ownership/recovery is present; builds were staged but not installed." >&2
    exit 1
}
[ ! -e "$DEV_ROOT/bin/herdr-mobile-relay" ] || mv "$DEV_ROOT/bin/herdr-mobile-relay" "$BUILD_DIR/herdr-mobile-relay.previous"
[ ! -e "$DEV_ROOT/web" ] || mv "$DEV_ROOT/web" "$BUILD_DIR/web.previous"
mv "$BUILD_DIR/herdr-mobile-relay" "$DEV_ROOT/bin/herdr-mobile-relay"
mv "$BUILD_DIR/web" "$DEV_ROOT/web"
# Preserve the previous generated build under BUILD_DIR for explicit inspection;
# never delete a user's existing state merely to rebuild the development app.

# Discard inherited production origins/credentials/transport choices. Tailscale
# identity comes only from the explicit CLI and the managed in-process owner.
unset HERDR_PLUGIN_CONFIG_DIR HERDR_GATEWAY_URL HERDR_GATEWAY_SELECTION HERDR_PHONE_APP_URL
unset HERDR_TAILSCALE_ORIGIN HERDR_RELAY_RUN_ID HERDR_RELAY_PAIRING_SOCKET GH_TOKEN
unset CURL_CA_BUNDLE SSL_CERT_FILE NODE_EXTRA_CA_CERTS
export HOME="$DEV_ROOT/home" XDG_CONFIG_HOME="$DEV_ROOT/config" XDG_CACHE_HOME="$DEV_ROOT/cache" XDG_DATA_HOME="$DEV_ROOT/data"
export HERDR_RELAY_ENV="$ENV_FILE" HERDR_RELEASE_ROOT="$DEV_ROOT/data/herdr-mobile-relay"
export HERDR_RELAY_BIN="$DEV_ROOT/bin/herdr-mobile-relay" HERDR_WEB_ROOT="$DEV_ROOT/web"
export HERDR_BIN="$HERDR_DEV_HERDR_BIN" HERDR_SOCKET_PATH="$HERDR_DEV_HERDR_SOCKET"
export HERDR_TAILSCALE_BIN="$HERDR_DEV_TAILSCALE_BIN" HERDR_TAILSCALE_HTTPS_PORT="$HERDR_DEV_TAILSCALE_HTTPS_PORT"
export HERDR_RELAY_PORT="$HERDR_DEV_TAILSCALE_PORT" HERDR_RELAY_PLUGIN_PORT="$HERDR_DEV_TAILSCALE_PLUGIN_PORT"
export HERDR_RELAY_HOST=127.0.0.1 HERDR_RELAY_REARM_BOOTSTRAP=0 HERDR_TAILSCALE_REQUEST=1
exec "$SCRIPT_DIR/tailscale.sh" "$@"
