#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
WRANGLER_VERSION="4.114.0"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="${HERDR_RELAY_ENV:-}"
if [ -z "$ENV_FILE" ]; then
    ENV_FILE="$(installed_service_env_file)"
fi
if [ -z "$ENV_FILE" ]; then
    echo "✗ Install the stable relay service before configuring app deployment." >&2
    exit 1
fi
ENV_FILE="$(canonical_file_path "$ENV_FILE")"
assert_service_env_matches "$ENV_FILE"
ensure_relay_env "$ENV_FILE"
load_relay_env "$ENV_FILE"

if ! NODE_DIR="$(node_bin_dir "$ENV_FILE")"; then
    echo "✗ Node.js and npx are required only on the relay that deploys the separate app." >&2
    echo "  This pane does not read your shell profile, so a node installed by" >&2
    echo "  nvm, fnm, volta, or asdf is not on its PATH. Looked in PATH," >&2
    echo "  \${NVM_DIR:-~/.nvm}/current/bin, the newest ~/.nvm/versions/node/*/bin," >&2
    echo "  ~/.local/share/fnm/aliases/default/bin, ~/.volta/bin, ~/.asdf/shims," >&2
    echo "  /opt/homebrew/bin, /usr/local/bin, ~/.local/bin, and /usr/bin." >&2
    echo "  Set HERDR_APP_DEPLOY_NODE_DIR in $ENV_FILE to the directory holding" >&2
    echo "  node and npx, or install Node.js 24, then rerun this action." >&2
    exit 1
fi
NODE_BIN="$NODE_DIR/node"
NPX_BIN="$NODE_DIR/npx"
echo "Using Node.js $("$NODE_BIN" --version) from $NODE_DIR"

RECORDED_ORIGIN="$(dirname "$ENV_FILE")/phone-app-origin"
DEFAULT_ORIGIN=""
if [ -r "$RECORDED_ORIGIN" ]; then
    DEFAULT_ORIGIN="$(head -1 "$RECORDED_ORIGIN")"
fi

echo "🐑 Configure Phone App Deployment"
echo ""
echo "This computer will be allowed to deploy one separately hosted Cloudflare"
echo "Pages app. It never sends Cloudflare credentials to the phone."
echo ""
if [ -n "$DEFAULT_ORIGIN" ]; then
    read -r -p "App origin [$DEFAULT_ORIGIN]: " APP_ORIGIN
    APP_ORIGIN="${APP_ORIGIN:-$DEFAULT_ORIGIN}"
else
    read -r -p "App origin (for example, app.example.com): " APP_ORIGIN
fi
if ! APP_ORIGIN="$(
    HERDR_PHONE_APP_URL="$APP_ORIGIN" phone_app_base_url "" "$ENV_FILE"
)"; then
    exit 1
fi

echo ""
echo "Checking Cloudflare Pages access..."
if ! PROJECTS_JSON="$(
    "$NPX_BIN" --yes "wrangler@$WRANGLER_VERSION" pages project list --json
)"; then
    echo ""
    echo "✗ Wrangler could not list Pages projects." >&2
    echo "  Run 'npx wrangler login' as this user, or set a scoped" >&2
    echo "  CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID in $ENV_FILE." >&2
    exit 1
fi

PROJECT_NAMES="$(
    printf '%s' "$PROJECTS_JSON" | "$(relay_binary)" pages-projects list
)"
if [ -z "$PROJECT_NAMES" ]; then
    echo "✗ No Cloudflare Pages projects are available to this account." >&2
    exit 1
fi
echo "$PROJECT_NAMES"

AVAILABLE_PROJECTS="$(
    printf '%s' "$PROJECTS_JSON" | "$(relay_binary)" pages-projects names
)"
MATCHING_PROJECTS="$(
    printf '%s' "$PROJECTS_JSON" | "$(relay_binary)" pages-projects matching "$APP_ORIGIN"
)"

DEFAULT_PROJECT=""
if [ -n "${HERDR_CLOUDFLARE_PAGES_PROJECT:-}" ] \
    && printf '%s\n' "$MATCHING_PROJECTS" \
        | grep -Fxq "$HERDR_CLOUDFLARE_PAGES_PROJECT"; then
    DEFAULT_PROJECT="$HERDR_CLOUDFLARE_PAGES_PROJECT"
elif [ "$(printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d' | wc -l)" -eq 1 ]; then
    DEFAULT_PROJECT="$(printf '%s\n' "$MATCHING_PROJECTS" | sed -n '1p')"
elif [ "$(printf '%s\n' "$AVAILABLE_PROJECTS" | sed '/^$/d' | wc -l)" -eq 1 ]; then
    DEFAULT_PROJECT="$(printf '%s\n' "$AVAILABLE_PROJECTS" | sed -n '1p')"
fi

echo ""
while true; do
    if [ -n "$DEFAULT_PROJECT" ]; then
        read -r -p "Pages project [$DEFAULT_PROJECT]: " PAGES_PROJECT
        PAGES_PROJECT="${PAGES_PROJECT:-$DEFAULT_PROJECT}"
    else
        read -r -p "Pages project name: " PAGES_PROJECT
    fi
    if ! printf '%s' "$PAGES_PROJECT" \
        | grep -Eq '^[a-z0-9]([a-z0-9-]{0,57}[a-z0-9])?$'; then
        echo "Project not available. Enter one of the project names shown above."
        continue
    fi

    if printf '%s' "$PROJECTS_JSON" \
        | "$(relay_binary)" pages-projects validate "$PAGES_PROJECT" "$APP_ORIGIN"; then
        break
    fi
    echo "Project unavailable or it does not serve $APP_ORIGIN. Choose a listed project with that domain."
done

set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_ORIGIN "$APP_ORIGIN"
set_env_value_atomic "$ENV_FILE" HERDR_CLOUDFLARE_PAGES_PROJECT "$PAGES_PROJECT"
set_env_value_atomic "$ENV_FILE" HERDR_CLOUDFLARE_PAGES_BRANCH "main"
set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_NPX "$NPX_BIN"
set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_NODE_DIR "$NODE_DIR"

echo ""
echo "Restarting the relay with app deployment enabled..."
"$SCRIPT_DIR/service.sh" install

echo ""
echo "✓ $PAGES_PROJECT may now deploy $APP_ORIGIN after confirmation from the phone."
echo "  Production branch: main"

CURRENT_VERSION="$(
    "$(relay_binary)" version --json \
        | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'
)"
echo ""
read -r -p "Deploy app version $CURRENT_VERSION now? [Y/n]: " DEPLOY_NOW
case "${DEPLOY_NOW:-y}" in
    y|Y|yes|YES)
        echo ""
        echo "Validating and publishing the committed app bundle..."
        set -a
        # shellcheck source=/dev/null
        . "$ENV_FILE"
        set +a
        if ! "$(relay_binary)" app-deploy-configured; then
            echo ""
            echo "✗ Initial app deployment failed. The authorization was preserved." >&2
            echo "  Rerun this action when you are ready to try again." >&2
            exit 1
        fi
        echo ""
        echo "✓ The public app is updated. Reopen it if the installed PWA does not reload."
        ;;
    n|N|no|NO)
        echo ""
        echo "No app was deployed. Rerun this action for the first deployment;"
        echo "later app releases can be deployed from Settings on the phone."
        ;;
    *)
        echo "✗ Enter y or n." >&2
        exit 1
        ;;
esac
