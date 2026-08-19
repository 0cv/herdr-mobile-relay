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

CONFIGURED_ORIGIN="$(dirname "$ENV_FILE")/phone-app-origin-configured"
OBSERVED_ORIGIN="$(dirname "$ENV_FILE")/phone-app-origin"
DEFAULT_ORIGIN="${HERDR_APP_DEPLOY_ORIGIN:-}"
if [ -z "$DEFAULT_ORIGIN" ] && [ -r "$CONFIGURED_ORIGIN" ]; then
    DEFAULT_ORIGIN="$(head -1 "$CONFIGURED_ORIGIN")"
elif [ -z "$DEFAULT_ORIGIN" ] && [ -r "$OBSERVED_ORIGIN" ]; then
    DEFAULT_ORIGIN="$(head -1 "$OBSERVED_ORIGIN")"
fi

echo "🐑 Configure Phone App Deployment"
echo ""
echo "This computer will be allowed to deploy one separately hosted Cloudflare"
echo "Pages app. It never sends Cloudflare credentials to the phone."
echo "Its app origin also becomes the shared origin used by this relay's setup QR."
echo "Use setup-menu option 7 instead when this computer should not own deployments."
echo ""

# The origin and the project have to agree: only a project that already serves
# that domain can deploy to it. Getting either wrong used to trap the person in
# a loop demanding a project that cannot exist, so both are asked inside one
# retry and either can be abandoned.
prompt_app_origin() {
    local entered

    while true; do
        if [ -n "$DEFAULT_ORIGIN" ]; then
            read -r -p "App origin [$DEFAULT_ORIGIN], or q to cancel: " entered ||
                return 1
            entered="${entered:-$DEFAULT_ORIGIN}"
        else
            read -r -p "App origin, for example app.example.com, or q to cancel: " entered ||
                return 1
        fi
        case "$entered" in
            q | Q) return 1 ;;
            '') continue ;;
        esac
        if APP_ORIGIN="$(
            HERDR_PHONE_APP_URL="$entered" phone_app_base_url "" "$ENV_FILE"
        )"; then
            return 0
        fi
    done
}

if ! prompt_app_origin; then
    echo "Configuration cancelled." >&2
    exit 1
fi

echo ""
echo "Checking Cloudflare Pages access..."
# npx is a #!/usr/bin/env node script, so the interpreter has to be findable
# too: calling it by absolute path is not enough when this pane's PATH has no
# node. The relay's own deployment worker prepends the same directory.
if ! PROJECTS_JSON="$(
    PATH="$NODE_DIR:$PATH" "$NPX_BIN" --yes "wrangler@$WRANGLER_VERSION" pages project list --json
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

# An origin nothing serves is an answer, not a failure, so a non-zero status
# here must not abort the script before it can say so.
MATCHING_PROJECTS="$(
    printf '%s' "$PROJECTS_JSON" | "$(relay_binary)" pages-projects matching "$APP_ORIGIN" || true
)"

# Wrangler 4 has no Pages custom-domain command, so attaching one means the REST
# API, which needs a token the OAuth login does not expose. With a token this
# does the work; without one it says exactly what to click. Either way the
# person is never told to go and fix something with no instructions.
cloudflare_account_id() {
    local token="$1"

    if [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]; then
        printf '%s\n' "$CLOUDFLARE_ACCOUNT_ID"
        return 0
    fi
    curl --fail --silent --show-error --max-time 15 \
        -H "Authorization: Bearer $token" \
        https://api.cloudflare.com/client/v4/accounts 2>/dev/null |
        grep -o '"id":"[0-9a-f]\{32\}"' | head -1 | cut -d'"' -f4
}

attach_pages_domain() {
    local project="$1"
    local host="${2#https://}"
    local token="${CLOUDFLARE_API_TOKEN:-}"
    local account
    local response

    [ -n "$token" ] || return 2
    account="$(cloudflare_account_id "$token")"
    [ -n "$account" ] || return 2
    if ! response="$(
        curl --fail --silent --show-error --max-time 30 -X POST \
            -H "Authorization: Bearer $token" \
            -H "Content-Type: application/json" \
            --data "{\"name\":\"$host\"}" \
            "https://api.cloudflare.com/client/v4/accounts/$account/pages/projects/$project/domains" 2>&1
    )"; then
        echo "✗ Cloudflare refused the domain: $response" >&2
        return 1
    fi
    return 0
}

# Attaching a domain changes the Cloudflare account, so it is never silent:
# a terminal is asked, and a script has to say so with
# HERDR_APP_DEPLOY_ATTACH_DOMAIN=true.
attach_requested() {
    local answer

    case "${HERDR_APP_DEPLOY_ATTACH_DOMAIN:-}" in
        true) return 0 ;;
        false) return 1 ;;
    esac
    [ -t 0 ] || return 1
    read -r -p "Add ${APP_ORIGIN#https://} to $ATTACH_PROJECT now? [Y/n]: " answer ||
        return 1
    case "${answer:-y}" in
        y | Y | yes | YES) return 0 ;;
        *) return 1 ;;
    esac
}

# No project serves this origin. Offer to attach it to one, and otherwise say
# which of the two things is wrong instead of asking the same question again.
if [ -z "$(printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d')" ]; then
    ACCOUNT_PROJECTS="$(
        printf '%s' "$PROJECTS_JSON" | "$(relay_binary)" pages-projects names || true
    )"
    ATTACH_PROJECT=""
    if [ "$(printf '%s\n' "$ACCOUNT_PROJECTS" | sed '/^$/d' | wc -l)" -eq 1 ]; then
        ATTACH_PROJECT="$(printf '%s\n' "$ACCOUNT_PROJECTS" | sed -n '1p')"
    fi
    echo ""
    echo "✗ No Pages project above serves $APP_ORIGIN." >&2
    if [ -n "${CLOUDFLARE_API_TOKEN:-}" ] && [ -n "$ATTACH_PROJECT" ] && attach_requested; then
        if attach_pages_domain "$ATTACH_PROJECT" "$APP_ORIGIN"; then
            echo "✓ Added ${APP_ORIGIN#https://} to $ATTACH_PROJECT."
            echo "  Cloudflare issues its certificate in the background."
            PROJECTS_JSON="$(
                PATH="$NODE_DIR:$PATH" "$NPX_BIN" --yes "wrangler@$WRANGLER_VERSION" \
                    pages project list --json
            )"
            MATCHING_PROJECTS="$(
                printf '%s' "$PROJECTS_JSON" |
                    "$(relay_binary)" pages-projects matching "$APP_ORIGIN" || true
            )"
        fi
    elif [ -z "${CLOUDFLARE_API_TOKEN:-}" ]; then
        echo "  This action can attach it for you with a Cloudflare API token that" >&2
        echo "  has the Pages:Edit permission: set CLOUDFLARE_API_TOKEN in" >&2
        echo "  $ENV_FILE and rerun. The wrangler login alone cannot do it." >&2
    fi
fi

if [ -z "$(printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d')" ]; then
    echo "  Otherwise add the domain in Cloudflare (Pages → the project →" >&2
    echo "  Custom domains), or enter an origin a listed project already serves." >&2
    echo "" >&2
    if ! prompt_app_origin; then
        echo "Configuration cancelled." >&2
        exit 1
    fi
    MATCHING_PROJECTS="$(
        printf '%s' "$PROJECTS_JSON" |
            "$(relay_binary)" pages-projects matching "$APP_ORIGIN" || true
    )"
    if [ -z "$(printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d')" ]; then
        echo "✗ No Pages project serves $APP_ORIGIN either. Nothing was changed." >&2
        exit 1
    fi
fi

DEFAULT_PROJECT=""
if [ -n "${HERDR_CLOUDFLARE_PAGES_PROJECT:-}" ] \
    && printf '%s\n' "$MATCHING_PROJECTS" \
        | grep -Fxq "$HERDR_CLOUDFLARE_PAGES_PROJECT"; then
    DEFAULT_PROJECT="$HERDR_CLOUDFLARE_PAGES_PROJECT"
elif [ "$(printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d' | wc -l)" -eq 1 ]; then
    DEFAULT_PROJECT="$(printf '%s\n' "$MATCHING_PROJECTS" | sed -n '1p')"
fi

echo ""
while true; do
    if [ -n "$DEFAULT_PROJECT" ]; then
        read -r -p "Pages project [$DEFAULT_PROJECT], or q to cancel: " PAGES_PROJECT ||
            PAGES_PROJECT=q
        PAGES_PROJECT="${PAGES_PROJECT:-$DEFAULT_PROJECT}"
    else
        read -r -p "Pages project name, or q to cancel: " PAGES_PROJECT || PAGES_PROJECT=q
    fi
    case "$PAGES_PROJECT" in
        q | Q)
            echo "Configuration cancelled." >&2
            exit 1
            ;;
    esac
    if ! printf '%s' "$PAGES_PROJECT" \
        | grep -Eq '^[a-z0-9]([a-z0-9-]{0,57}[a-z0-9])?$'; then
        echo "Enter one of the project names listed above, or q to cancel."
        continue
    fi

    if printf '%s' "$PROJECTS_JSON" \
        | "$(relay_binary)" pages-projects validate "$PAGES_PROJECT" "$APP_ORIGIN"; then
        break
    fi
    echo "$PAGES_PROJECT cannot deploy $APP_ORIGIN. These can:"
    printf '%s\n' "$MATCHING_PROJECTS" | sed '/^$/d;s/^/  /'
done

set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_ORIGIN "$APP_ORIGIN"
set_env_value_atomic "$ENV_FILE" HERDR_CLOUDFLARE_PAGES_PROJECT "$PAGES_PROJECT"
set_env_value_atomic "$ENV_FILE" HERDR_CLOUDFLARE_PAGES_BRANCH "main"
set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_NPX "$NPX_BIN"
set_env_value_atomic "$ENV_FILE" HERDR_APP_DEPLOY_NODE_DIR "$NODE_DIR"
record_phone_app_origin "$APP_ORIGIN" "$ENV_FILE"

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
