#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-common-gateway-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

ENV_FILE="$WORK_DIR/config/relay.env"
mkdir -p "$(dirname "$ENV_FILE")"
EXPECTED_TOKEN="test-private-token"
GH_TOKEN="$EXPECTED_TOKEN"
GITHUB_TOKEN="$EXPECTED_TOKEN"
export GH_TOKEN GITHUB_TOKEN
ensure_relay_env "$ENV_FILE"

# The setup fragment carries only gateways=<ordered list> for a
# gateway-configured relay and relay=<wss url> otherwise. Both keep the token
# inside the fragment.
FRAGMENT_BIN="$WORK_DIR/fragment/herdr-mobile-relay"
mkdir -p "$(dirname "$FRAGMENT_BIN")"
cat > "$FRAGMENT_BIN" <<'EOF'
#!/bin/sh
# Stands in for `herdr-mobile-relay setup-fragment TOKEN LABEL [RELAY]`:
# alphabetically sorted keys with percent-encoded values.
test "$1" = "setup-fragment" || exit 2
encoded="$(printf '%s' "$4" | sed -e 's|:|%3A|g' -e 's|/|%2F|g')"
if [ -z "$4" ]; then
    printf 'label=%s&setup=%s\n' "$3" "$2"
else
    printf 'label=%s&relay=%s&setup=%s\n' "$3" "$encoded" "$2"
fi
EOF
chmod 700 "$FRAGMENT_BIN"

GATEWAY_ENV="$WORK_DIR/config/gateway.env"
printf "HERDR_GATEWAY_URL='wss://gw.example.test/'\n" > "$GATEWAY_ENV"
GATEWAY_FRAGMENT="$(
    unset HERDR_GATEWAY_URL
    HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation "wss://relay.example.test"
)"
case "$GATEWAY_FRAGMENT" in
    *"gateways=wss%3A%2F%2Fgw.example.test"*) ;;
    *)
        echo "gateway fragment lost the candidate list: $GATEWAY_FRAGMENT" >&2
        exit 1
        ;;
esac
case "$GATEWAY_FRAGMENT" in
    *relay=*)
        echo "gateway fragment still advertises a relay URL: $GATEWAY_FRAGMENT" >&2
        exit 1
        ;;
esac
case "$GATEWAY_FRAGMENT" in
    *"setup=relay-secret-token"*) ;;
    *)
        echo "gateway fragment dropped the relay token" >&2
        exit 1
        ;;
esac

TUNNEL_FRAGMENT="$(
    unset HERDR_GATEWAY_URL
    HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$WORK_DIR/config/relay.env" \
        build_transport_setup_fragment relay-secret-token workstation "wss://relay.example.test"
)"
test "$TUNNEL_FRAGMENT" = "label=workstation&relay=wss%3A%2F%2Frelay.example.test&setup=relay-secret-token"

# The environment wins over the env file, so a one-off gateway override works.
# gateways= carries even a single entry: the phone then needs no re-scan when a
# second gateway is added later.
ENV_GATEWAY_FRAGMENT="$(
    HERDR_GATEWAY_URL="wss://other.example.test" \
        HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation ""
)"
test "$ENV_GATEWAY_FRAGMENT" = "label=workstation&setup=relay-secret-token&gateways=wss%3A%2F%2Fother.example.test"

# An ordered candidate list is encoded in full, in order, so a phone fails over
# to the second gateway on its own.
LIST_GATEWAY_FRAGMENT="$(
    HERDR_GATEWAY_URL="wss://a.example.test, wss://b.example.test/" \
        HERDR_RELAY_BIN="$FRAGMENT_BIN" HERDR_RELAY_ENV="$GATEWAY_ENV" \
        build_transport_setup_fragment relay-secret-token workstation ""
)"
test "$LIST_GATEWAY_FRAGMENT" = "label=workstation&setup=relay-secret-token&gateways=wss%3A%2F%2Fa.example.test,wss%3A%2F%2Fb.example.test"

test "$(HERDR_GATEWAY_URL="wss://gw.example.test/" gateway_url "$WORK_DIR/config/relay.env")" = "wss://gw.example.test"
test -z "$(unset HERDR_GATEWAY_URL; gateway_url "$WORK_DIR/config/relay.env")"

# A list is parsed in order, with blank entries and trailing slashes dropped, and
# gateway_url stays the first entry used by setup and service scripts.
test "$(HERDR_GATEWAY_URL=" wss://a.example.test ,, wss://b.example.test/ ," gateway_urls "$GATEWAY_ENV")" = "wss://a.example.test,wss://b.example.test"
test "$(HERDR_GATEWAY_URL="wss://a.example.test,wss://b.example.test" gateway_url "$GATEWAY_ENV")" = "wss://a.example.test"
test -z "$(unset HERDR_GATEWAY_URL; gateway_urls "$WORK_DIR/config/relay.env")"

# The gateway URL normalizer delegates to the compiled origin normalizer, so it
# is stubbed the same way the fragment helper is above.
NORMALIZE_BIN="$WORK_DIR/normalize/herdr-mobile-relay"
mkdir -p "$(dirname "$NORMALIZE_BIN")"
cat > "$NORMALIZE_BIN" <<'EOF'
#!/bin/sh
# Stands in for `herdr-mobile-relay normalize-origin --allow-loopback-http URL`:
# a bare host defaults to HTTPS, plain HTTP is loopback-only, and credentials,
# paths, queries, and fragments are rejected.
test "$1" = "normalize-origin" || exit 2
test "$2" = "--allow-loopback-http" || exit 2
value="$3"
case "$value" in
    *://*) ;;
    *) value="https://$value" ;;
esac
scheme="${value%%://*}"
host="${value#*://}"
host="${host%/}"
case "$host" in
    ''|*/*|*\?*|*'#'*|*@*) exit 1 ;;
esac
case "$scheme" in
    https) ;;
    http)
        case "${host%%:*}" in
            localhost|127.0.0.1|::1) ;;
            *) exit 1 ;;
        esac
        ;;
    *) exit 1 ;;
esac
printf '%s://%s\n' "$scheme" "$host"
EOF
chmod 700 "$NORMALIZE_BIN"
export HERDR_RELAY_BIN="$NORMALIZE_BIN"

test "$(normalize_gateway_url gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url https://gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url wss://gw.example.com)" = "wss://gw.example.com"
test "$(normalize_gateway_url http://127.0.0.1:8443)" = "ws://127.0.0.1:8443"
test "$(normalize_gateway_urls "gw.example.com, https://backup.example.com, wss://gw.example.com")" = \
    "wss://gw.example.com,wss://backup.example.com"
for REJECTED_LIST in "gw.example.com,gw.example.com/x" ","; do
    if normalize_gateway_urls "$REJECTED_LIST" >/dev/null 2>&1; then
        echo "gateway list normalizer accepted '$REJECTED_LIST'" >&2
        exit 1
    fi
done
for REJECTED in "gw.example.com/x" "user:pw@gw.example.com" ""; do
    if normalize_gateway_url "$REJECTED" >/dev/null 2>&1; then
        echo "gateway URL normalizer accepted '$REJECTED'" >&2
        exit 1
    fi
done
unset HERDR_RELAY_BIN

test "$(gateway_http_base wss://gw.example.test)" = "https://gw.example.test"
test "$(gateway_http_base ws://127.0.0.1:8443)" = "http://127.0.0.1:8443"

# Writing the transport choice is what the chooser does; clearing it returns the
# relay to the Cloudflare tunnel path.
CHOICE_ENV="$WORK_DIR/config/choice.env"
set_gateway_url "$CHOICE_ENV" "wss://gw.example.test"
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_URL)" = "wss://gw.example.test"
test "$(unset HERDR_GATEWAY_URL; gateway_url "$CHOICE_ENV")" = "wss://gw.example.test"

# The selection policy is a second, independent switch with exactly two legal
# values. Anything else is refused without touching the file, because writing a
# policy nobody understands would silently change which gateway carries traffic.
set_gateway_selection "$CHOICE_ENV" ordered
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_SELECTION)" = "ordered"
set_gateway_selection "$CHOICE_ENV" latency
test "$(env_file_value "$CHOICE_ENV" HERDR_GATEWAY_SELECTION)" = "latency"
SELECTION_ENV_BEFORE="$(cat "$CHOICE_ENV")"
for REJECTED_SELECTION in "fastest" "Ordered" "ordered latency" ""; do
    if set_gateway_selection "$CHOICE_ENV" "$REJECTED_SELECTION"; then
        echo "gateway selection accepted '$REJECTED_SELECTION'" >&2
        exit 1
    fi
    test "$(cat "$CHOICE_ENV")" = "$SELECTION_ENV_BEFORE"
done

# Leaving the gateway path drops the policy along with the list, so a later
# choice cannot inherit a stale one.
set_gateway_url "$CHOICE_ENV" ""
if grep -qE '^HERDR_GATEWAY_(URL|SELECTION)=' "$CHOICE_ENV"; then
    echo "clearing the gateway URL left a gateway key behind" >&2
    exit 1
fi
test -z "$(unset HERDR_GATEWAY_URL; gateway_url "$CHOICE_ENV")"

# The community gateway is published, so an install that configures nothing gets
# the shared one; an operator overrides it, and an explicitly empty value is the
# documented way to say "this build runs no community gateway".
test "$(unset HERDR_COMMUNITY_GATEWAY_URL; community_gateway_url)" = \
    "wss://gw1.herdr-mobile.dev,wss://gw2.herdr-mobile.dev"
test "$(HERDR_COMMUNITY_GATEWAY_URL="wss://community.example.test" community_gateway_url)" = "wss://community.example.test"
test "$(HERDR_COMMUNITY_GATEWAY_URL="wss://a.example.test,wss://b.example.test" community_gateway_url)" = \
    "wss://a.example.test,wss://b.example.test"

# An own gateway always reaches an explicit final candidate-list choice. The
# default keeps the operator entry first, appends every community fallback, and
# removes duplicates without reordering.
test "$(
    HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_COMMUNITY_GATEWAY_URL="wss://gw-a.example.test,wss://gw-b.example.test" \
        gateway_subscription_defaults "wss://own.example.test,wss://gw-a.example.test"
)" = "wss://own.example.test,wss://gw-a.example.test,wss://gw-b.example.test"
test "$(
    HERDR_RELAY_BIN="$NORMALIZE_BIN" \
        HERDR_GATEWAY_SUBSCRIPTIONS="wss://own.example.test,wss://backup.example.test" \
        prompt_gateway_subscriptions "wss://unused.example.test"
)" = "wss://own.example.test,wss://backup.example.test"
test -z "$(HERDR_COMMUNITY_GATEWAY_URL="" community_gateway_url)"

# The selection rule is a decision about which gateway carries traffic, so an
# unrecognized or empty answer keeps the default the list was built with rather
# than switching policies behind the operator's back.
test "$(gateway_selection_choice ordered '')" = "ordered"
test "$(gateway_selection_choice ordered 1)" = "ordered"
test "$(gateway_selection_choice ordered 2)" = "latency"
test "$(gateway_selection_choice latency '')" = "latency"
test "$(gateway_selection_choice latency 2)" = "ordered"
test "$(gateway_selection_choice ordered nonsense)" = "ordered"
# Without a terminal the action's own default stands. A saved policy in the
# environment must not answer for the operator: the setup menu loads relay.env
# before running an action, so honouring it here would let an old own-gateway
# `ordered` silently follow them into a freshly chosen community list.
test "$(prompt_gateway_selection ordered < /dev/null)" = "ordered"
test "$(HERDR_GATEWAY_SELECTION=ordered prompt_gateway_selection latency < /dev/null)" = "latency"
test "$(HERDR_GATEWAY_SELECTION=latency prompt_gateway_selection ordered < /dev/null)" = "ordered"
# The status line phrases the rule instead of printing the variable.
test "$(gateway_selection_label latency)" = "closest wins"
test "$(gateway_selection_label ordered)" = "first listed wins"
test "$(gateway_selection_label '')" = "first listed wins"

echo "common gateway shell tests passed"
