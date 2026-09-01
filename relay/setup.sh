#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WEB_ENV_FILE="$REPO_DIR/.env"
INSTALL_MISSING=0
CLOUDFLARED_TEMP_DIR=""

export PATH="/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"

require_supported_platform

case "${1:-}" in
    "") ;;
    --install-missing) INSTALL_MISSING=1 ;;
    *)
        echo "Usage: $0 [--install-missing]"
        exit 2
        ;;
esac

cleanup() {
    if [ -n "$CLOUDFLARED_TEMP_DIR" ] && [ -d "$CLOUDFLARED_TEMP_DIR" ]; then
        rm -rf "$CLOUDFLARED_TEMP_DIR"
    fi
}
trap cleanup EXIT

install_cloudflared() {
    local system
    local machine
    local asset
    local source
    system="$(uname -s)"
    machine="$(uname -m)"

    case "$system:$machine" in
        Linux:x86_64|Linux:amd64) asset="cloudflared-linux-amd64" ;;
        Linux:aarch64|Linux:arm64) asset="cloudflared-linux-arm64" ;;
        Linux:armv7l|Linux:armv6l) asset="cloudflared-linux-arm" ;;
        Darwin:x86_64|Darwin:amd64) asset="cloudflared-darwin-amd64.tgz" ;;
        Darwin:arm64|Darwin:aarch64) asset="cloudflared-darwin-arm64.tgz" ;;
        *)
            echo "No automatic cloudflared download is available for $system $machine."
            return 1
            ;;
    esac

    CLOUDFLARED_TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-cloudflared.XXXXXX")"
    echo "Installing cloudflared from Cloudflare's official release..."
    curl --fail --location --silent --show-error \
        "https://github.com/cloudflare/cloudflared/releases/latest/download/$asset" \
        --output "$CLOUDFLARED_TEMP_DIR/$asset"
    source="$CLOUDFLARED_TEMP_DIR/$asset"
    if [ "$system" = "Darwin" ]; then
        tar -xzf "$source" -C "$CLOUDFLARED_TEMP_DIR"
        source="$CLOUDFLARED_TEMP_DIR/cloudflared"
    fi
    mkdir -p "$HOME/.local/bin"
    install -m 0755 "$source" "$HOME/.local/bin/cloudflared"
    rm -rf "$CLOUDFLARED_TEMP_DIR"
    CLOUDFLARED_TEMP_DIR=""
}

install_tool() {
    case "$1" in
        herdr)
            echo "Installing Herdr from herdr.dev..."
            curl -fsSL https://herdr.dev/install.sh | sh
            ;;
        relay-release)
            version="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml")"
            [ -n "$version" ] || return 1
            sh "$REPO_DIR/install.sh" "$version"
            ;;
        cloudflared) install_cloudflared ;;
    esac
}

GATEWAY_URL="$(gateway_url "$ENV_FILE")"

missing_tools=()
find_missing_tools() {
    missing_tools=()
    local command
    local required=(herdr)
    # A gateway-configured relay dials the gateway itself, so cloudflared is
    # neither installed nor required on that path.
    if [ -z "$GATEWAY_URL" ]; then
        required+=(cloudflared)
    fi
    for command in "${required[@]}"; do
        if [ "$command" = "herdr" ] && [ -x "${HERDR_BIN:-}" ]; then
            continue
        fi
        if ! command -v "$command" >/dev/null 2>&1; then
            missing_tools+=("$command")
        fi
    done
    if ! relay_binary >/dev/null 2>&1; then
        missing_tools+=("relay-release")
    fi
}

if [ -n "$GATEWAY_URL" ]; then
    echo "Gateway transport configured: $GATEWAY_URL"
    echo "Skipping the cloudflared prerequisite; no Cloudflare account is needed."
    echo ""
fi

find_missing_tools
if [ "${#missing_tools[@]}" -ne 0 ] && [ "$INSTALL_MISSING" -eq 1 ]; then
    if ! command -v curl >/dev/null 2>&1; then
        echo "curl is required to install: ${missing_tools[*]}"
        exit 1
    fi
    answer="${HERDR_SETUP_YES:-}"
    if [ "$answer" != "1" ]; then
        if [ ! -t 0 ]; then
            echo "Missing required tools: ${missing_tools[*]}"
            echo "Run this command in an interactive terminal, or set HERDR_SETUP_YES=1."
            exit 1
        fi
        printf 'Install these missing tools for your user account: %s? [Y/n] ' "${missing_tools[*]}"
        read -r answer
    fi
    case "$answer" in
        ""|1|y|Y|yes|YES)
            for command in "${missing_tools[@]}"; do
                install_tool "$command"
            done
            hash -r
            ;;
        *)
            echo "Installation cancelled."
            exit 1
            ;;
    esac
    find_missing_tools
fi

if [ "${#missing_tools[@]}" -ne 0 ]; then
    printf 'Missing required commands:'
    printf ' %s' "${missing_tools[@]}"
    echo ""
    if [ -n "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
        echo "Run Herdr Mobile Relay: Quick Start again to install them interactively, or install them yourself:"
    else
        echo "Run make quick-start to install them interactively, or install them yourself:"
    fi
    echo "  Herdr:       https://herdr.dev"
    echo "  Relay:       rerun the exact version installer from this plugin"
    if [ -z "$GATEWAY_URL" ]; then
        echo "  cloudflared: https://developers.cloudflare.com/tunnel/downloads/"
    fi
    exit 1
fi

# Voices are cached outside the release, so this download happens once per
# computer rather than once per update. The relay binary owns the catalog and
# does the downloading, which the prerequisite check above already required.
speech_missing="$("$SCRIPT_DIR/speech-voices.sh" --missing)"
if [ -n "$speech_missing" ]; then
    if [ "$INSTALL_MISSING" -eq 1 ]; then
        answer="${HERDR_SETUP_YES:-}"
        if [ "$answer" != "1" ] && [ -t 0 ]; then
            printf 'Download the English voice that reads responses aloud (about 115 MB with the engine, cached across updates)? [Y/n] '
            read -r answer
        fi
        case "$answer" in
            ""|1|y|Y|yes|YES)
                "$SCRIPT_DIR/speech-voices.sh" ||
                    echo "Speech voices were not installed; reading aloud falls back to the system speech engine."
                ;;
            *) echo "Skipped the speech voice. Run relay/speech-voices.sh later, or download it from the phone's Settings." ;;
        esac
    else
        echo "Optional: run relay/speech-voices.sh to cache the English voice that reads responses aloud."
    fi
fi

if [ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ] && [ ! -f "$WEB_ENV_FILE" ]; then
    install -m 0600 "$REPO_DIR/.env.example" "$WEB_ENV_FILE"
    echo "Created $WEB_ENV_FILE"
fi
ensure_relay_env "$ENV_FILE"

if ! command -v npx >/dev/null 2>&1; then
    echo "Optional: install Node.js/npm only if you want to self-host the web app."
fi

echo ""
echo "Prerequisites and local configuration are ready."
echo "  Relay config: $ENV_FILE"
if [ -n "$GATEWAY_URL" ]; then
    echo "  Gateway:      $GATEWAY_URL"
fi
if [ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
    echo "  Web config:   $WEB_ENV_FILE"
fi
if [ "$INSTALL_MISSING" -eq 0 ] && [ -z "${HERDR_PLUGIN_CONFIG_DIR:-}" ]; then
    echo ""
    echo "Next: run make quick-start"
fi
