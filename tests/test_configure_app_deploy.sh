#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-configure-app-deploy-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

# A phone-app origin no Pages project serves used to demand a project that
# could not exist, with no way out but killing the pane.
DEPLOY_HOME="$WORK_DIR/deploy-home"
DEPLOY_BIN="$WORK_DIR/deploy-bin"
DEPLOY_ENV="$DEPLOY_HOME/relay.env"
mkdir -p "$DEPLOY_HOME" "$DEPLOY_BIN"
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
write_stub "$DEPLOY_BIN/relay-stub" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "normalize-origin --allow-loopback-http")
        host="${3#https://}"
        printf 'https://%s\n' "${host#http://}"
        ;;
    "pages-projects list") printf '  herdr-0cv (herdr-0cv.pages.dev, app.example.test)\n' ;;
    "pages-projects names") printf 'herdr-0cv\n' ;;
    "pages-projects matching")
        if [ "$3" = "https://app.example.test" ] ||
            { [ -f "$DEPLOY_ATTACHED" ] && [ "$3" = "https://$(cat "$DEPLOY_ATTACHED")" ]; }; then
            printf 'herdr-0cv\n'
        fi
        exit 0
        ;;
    "pages-projects validate")
        test "$3" = herdr-0cv || exit 1
        test "$4" = "https://app.example.test" ||
            { [ -f "$DEPLOY_ATTACHED" ] && [ "$4" = "https://$(cat "$DEPLOY_ATTACHED")" ]; }
        ;;
    *) exit 2 ;;
esac
EOF
printf '#!/bin/sh\nprintf "{}\\n"\n' > "$DEPLOY_BIN/npx"
write_stub "$DEPLOY_BIN/node" <<'EOF'
#!/bin/sh
[ "$1" = --version ] && { echo "v26.1.0"; exit 0; }
exit 0
EOF
# Stands in for the Cloudflare API: records the attach request and reports the
# domain as served from then on.
write_stub "$DEPLOY_BIN/curl" <<'EOF'
#!/bin/sh
for argument in "$@"; do
    case "$argument" in
        *"/pages/projects/"*"/domains")
            printf '%s\n' "$argument" > "$DEPLOY_ATTACH_LOG"
            ;;
        '{"name":"'*)
            printf '%s' "$argument" | sed 's/.*"name":"//;s/".*//' > "$DEPLOY_ATTACHED"
            ;;
    esac
done
printf '{"success":true}\n'
EOF
chmod 700 "$DEPLOY_BIN/npx"
DEPLOY_ATTACHED="$WORK_DIR/attached-domain"
DEPLOY_ATTACH_LOG="$WORK_DIR/attach-url"
export DEPLOY_ATTACHED DEPLOY_ATTACH_LOG

run_configure_app_deploy() {
    printf '%b' "$1" | HOME="$DEPLOY_HOME" \
        PATH="$DEPLOY_BIN:$PATH" \
        HERDR_RELAY_BIN="$DEPLOY_BIN/relay-stub" \
        HERDR_APP_DEPLOY_NODE_DIR="$DEPLOY_BIN" \
        HERDR_RELAY_ENV="$DEPLOY_ENV" \
        bash "$REPO_DIR/relay/configure-app-deploy.sh" 2>&1
}

DEPLOY_OUTPUT="$(run_configure_app_deploy 'unserved.example.test\nq\n' || true)"
case "$DEPLOY_OUTPUT" in
    *"No Pages project above serves https://unserved.example.test"*"Configuration cancelled."*) ;;
    *)
        echo "app deploy did not explain or escape an unservable origin" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
if grep -q 'HERDR_CLOUDFLARE_PAGES_PROJECT' "$DEPLOY_ENV"; then
    echo "a cancelled app deploy configuration still wrote to relay.env" >&2
    exit 1
fi

# Correcting the origin at that second chance has to get past the origin problem
# and reach the project question, which is itself escapable. Prompts are
# invisible here because bash prints them only to a terminal, so the evidence is
# what the run did: it stopped at the project step without complaining about the
# origin again, and wrote nothing.
DEPLOY_OUTPUT="$(run_configure_app_deploy 'unserved.example.test\napp.example.test\nq\n' || true)"
case "$DEPLOY_OUTPUT" in
    *"No Pages project serves https://app.example.test either"*)
        echo "a corrected origin was rejected" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
case "$DEPLOY_OUTPUT" in
    *"Configuration cancelled."*) ;;
    *)
        echo "the project question could not be escaped" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
if grep -q 'HERDR_CLOUDFLARE_PAGES_PROJECT' "$DEPLOY_ENV"; then
    echo "escaping the project question still wrote to relay.env" >&2
    exit 1
fi

# The whole point of the flow: a matching origin and project are recorded.
printf '#!/bin/sh\nexit 0\n' > "$DEPLOY_BIN/systemctl"
chmod 700 "$DEPLOY_BIN/systemctl"
run_configure_app_deploy 'app.example.test\nherdr-0cv\n' >/dev/null 2>&1 || true
test "$(env_file_value "$DEPLOY_ENV" HERDR_APP_DEPLOY_ORIGIN)" = "https://app.example.test"
test "$(env_file_value "$DEPLOY_ENV" HERDR_CLOUDFLARE_PAGES_PROJECT)" = "herdr-0cv"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin-configured")" = "https://app.example.test"
printf 'https://relay.example.test\n' > "$(dirname "$DEPLOY_ENV")/phone-app-origin"
DEPLOY_REOPEN_OUTPUT="$(
    run_configure_app_deploy '\nherdr-0cv\nn\n' 2>&1 || true
)"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin-configured")" = "https://app.example.test"
test "$(cat "$(dirname "$DEPLOY_ENV")/phone-app-origin")" = "https://relay.example.test"

# With a token, the action attaches the domain itself instead of sending the
# person to the dashboard, and then records the configuration it just made
# possible.
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
rm -f "$DEPLOY_ATTACHED" "$DEPLOY_ATTACH_LOG"
CLOUDFLARE_API_TOKEN=test-token \
    CLOUDFLARE_ACCOUNT_ID=0123456789abcdef0123456789abcdef \
    HERDR_APP_DEPLOY_ATTACH_DOMAIN=true \
    run_configure_app_deploy 'new.example.test\nherdr-0cv\n' >/dev/null 2>&1 || true
test "$(cat "$DEPLOY_ATTACHED" 2>/dev/null)" = "new.example.test" ||
    { echo "the domain was never sent to Cloudflare" >&2; exit 1; }
case "$(cat "$DEPLOY_ATTACH_LOG" 2>/dev/null)" in
    *"/accounts/0123456789abcdef0123456789abcdef/pages/projects/herdr-0cv/domains") ;;
    *)
        echo "the attach request went to the wrong endpoint" >&2
        cat "$DEPLOY_ATTACH_LOG" >&2 || true
        exit 1
        ;;
esac
test "$(env_file_value "$DEPLOY_ENV" HERDR_APP_DEPLOY_ORIGIN)" = "https://new.example.test"

# Without a token it must not pretend: it says what to set, and changes nothing.
printf "HERDR_RELAY_TOKEN='deploy-token'\n" > "$DEPLOY_ENV"
rm -f "$DEPLOY_ATTACHED" "$DEPLOY_ATTACH_LOG"
DEPLOY_OUTPUT="$(
    HERDR_APP_DEPLOY_ATTACH_DOMAIN=true \
        run_configure_app_deploy 'other.example.test\nq\n' || true
)"
case "$DEPLOY_OUTPUT" in
    *"set CLOUDFLARE_API_TOKEN in"*) ;;
    *)
        echo "app deploy did not name the credential it needs to attach a domain" >&2
        printf '%s\n' "$DEPLOY_OUTPUT" >&2
        exit 1
        ;;
esac
[ ! -e "$DEPLOY_ATTACHED" ] ||
    { echo "a domain was attached without a token" >&2; exit 1; }

echo "configure-app-deploy shell tests passed"
