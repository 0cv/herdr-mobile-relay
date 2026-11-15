#!/usr/bin/env bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-change-hostname-test.XXXXXX")"
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT

# shellcheck source=../relay/common.sh
. "$REPO_DIR/relay/common.sh"
# shellcheck source=lib.sh
. "$REPO_DIR/tests/lib.sh"

unset GH_TOKEN GITHUB_TOKEN
ensure_relay_test_binary "$WORK_DIR"

# Moving a relay to a new domain must not mean tearing down the tunnel and
# re-pairing every phone: the route is added to the existing tunnel, the ingress
# follows, and the recorded hostname keeps up so a later teardown still matches.
MOVE_HOME="$WORK_DIR/move-home"
MOVE_BIN="$WORK_DIR/move-bin"
MOVE_ENV="$MOVE_HOME/relay.env"
MOVE_CONFIG="$MOVE_HOME/cloudflared/config.yml"
MOVE_STATE="$MOVE_HOME/stable-setup.json"
MOVE_ROUTE_LOG="$WORK_DIR/route-log"
mkdir -p "$MOVE_HOME/cloudflared" "$MOVE_BIN"
export MOVE_ROUTE_LOG
printf "HERDR_RELAY_TOKEN='move-token'\nCLOUDFLARED_CONFIG='%s'\n" "$MOVE_CONFIG" > "$MOVE_ENV"
cat > "$MOVE_CONFIG" <<'EOF'
tunnel: 'herdr-mobile-relay-fedora' # existing tunnel
credentials-file: /somewhere/creds.json
ingress:
  - hostname: "relay-fedora.old.test" # existing route
    service: http://localhost:8375
  - service: http_status:404
EOF
printf '{"owner":"herdr-mobile-relay-stable-setup-v1","schema":1,"hostname":"relay-fedora.old.test"}\n' \
    > "$MOVE_STATE"
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$MOVE_ROUTE_LOG"
exit 0
EOF
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
case "$*" in
    "--help all")
        printf '%s\n' '     --doh-url <URL>  Resolve host names over DNS-over-HTTPS'
        exit 0
        ;;
    *"relay-fedora.new.test"*)
        case "$*" in
            *"--doh-url "*) ;;
            *) exit 6 ;;
        esac
        ;;
    *"relay-fedora.unreachable.test"*) exit 6 ;;
esac
printf '{"status":"ok","instance":"same","version":"9.9.9","protocol":2}\n'
EOF
cat > "$MOVE_BIN/relay-stub" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "stable-state health-match") exit 0 ;;
    "stable-state update")
        printf '%s\n' "${4#hostname=}" > "$MOVE_STATE_RECORD"
        exit 0
        ;;
    *) exit 0 ;;
esac
EOF
printf '#!/bin/sh\nexit 0\n' > "$MOVE_BIN/service.sh"
# change-hostname.sh reinstalls the service with the repository's service.sh,
# so report it as not loaded instead of letting that reach this machine's relay.
cat > "$MOVE_BIN/launchctl" <<'EOF'
#!/bin/sh
case "$1 $2" in
    "print gui/"*/*) exit 1 ;;
esac
EOF
chmod 700 "$MOVE_BIN/cloudflared" "$MOVE_BIN/curl" "$MOVE_BIN/relay-stub" "$MOVE_BIN/service.sh" \
    "$MOVE_BIN/launchctl"
MOVE_STATE_RECORD="$WORK_DIR/state-hostname"
export MOVE_STATE_RECORD
reset_move_config() {
    cat > "$MOVE_CONFIG" <<'YAML'
tunnel: herdr-mobile-relay-fedora
credentials-file: /somewhere/creds.json
ingress:
  - hostname: relay-fedora.old.test
    service: http://localhost:8375
  - service: http_status:404
YAML
    printf "HERDR_RELAY_TOKEN='move-token'\nCLOUDFLARED_CONFIG='%s'\n" "$MOVE_CONFIG" > "$MOVE_ENV"
}
# setup-link and service.sh are the two things this action hands off to; both
# are stubbed so the test observes the move itself.
cp "$MOVE_BIN/service.sh" "$WORK_DIR/move-service.sh"

LINK_OUTPUT="$(
    HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN" HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_PHONE_APP_URL=relay bash "$REPO_DIR/relay/setup-link.sh"
)"
grep -Fq 'relay=wss%3A%2F%2Frelay-fedora.old.test&' <<< "$LINK_OUTPUT"
LINK_OUTPUT="$(
    HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$HERDR_TEST_RELAY_BIN" HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_PHONE_APP_URL=relay bash "$REPO_DIR/relay/setup-link.sh" \
        'WSS://RELAY-FEDORA.OLD.TEST./setup'
)"
grep -Fq 'relay=wss%3A%2F%2Frelay-fedora.old.test&' <<< "$LINK_OUTPUT"

if HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" \
    HERDR_RELAY_ENV="$MOVE_ENV" HERDR_STABLE_HOSTNAME=relay.-bad.test \
    bash "$REPO_DIR/relay/change-hostname.sh" > "$WORK_DIR/invalid-hostname" 2>&1; then
    echo "an invalid unattended hostname was accepted" >&2
    exit 1
fi
grep -Fq 'Enter a valid hostname' "$WORK_DIR/invalid-hostname"
test ! -s "$MOVE_ROUTE_LOG"

MOVE_OUTPUT="$(
    printf 'relay.-bad.test\nHTTPS://RELAY-FEDORA.NEW.TEST./setup\n' |
        HOME="$MOVE_HOME" \
        PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" \
        HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_STABLE_STATE_FILE="$MOVE_STATE" \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
grep -Fq 'tunnel route dns herdr-mobile-relay-fedora relay-fedora.new.test' "$MOVE_ROUTE_LOG" ||
    { echo "the new hostname was never routed to the existing tunnel" >&2
      printf '%s\n' "$MOVE_OUTPUT" >&2; exit 1; }
grep -Fq 'hostname: "relay-fedora.new.test" # existing route' "$MOVE_CONFIG" ||
    { echo "the ingress still serves the old hostname" >&2; exit 1; }
grep -Fq 'hostname: "relay-fedora.old.test" # existing route' "$MOVE_CONFIG" ||
    { echo "the old hostname stopped serving before its record was deleted" >&2; exit 1; }
grep -Fq "tunnel: 'herdr-mobile-relay-fedora' # existing tunnel" "$MOVE_CONFIG" ||
    { echo "the move rewrote more than the hostname" >&2; exit 1; }
test "$(cat "$MOVE_STATE_RECORD" 2>/dev/null)" = "relay-fedora.new.test" ||
    { echo "the recorded hostname was not updated" >&2; exit 1; }
[ -f "$MOVE_CONFIG.herdr-previous" ] ||
    { echo "the previous config was not kept" >&2; exit 1; }

# A gateway relay has no hostname to move, and saying so beats editing a config
# that is not in use.
printf "HERDR_RELAY_TOKEN='move-token'\n" > "$MOVE_ENV"
MOVE_OUTPUT="$(
    HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" \
        HERDR_RELAY_ENV="$MOVE_ENV" bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"does not run a Cloudflare tunnel"*) ;;
    *)
        echo "a gateway relay was not told hostnames do not apply" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac

# cloudflared exits 0 while creating a different name when its certificate does
# not cover the zone. Believing it once left a live relay serving a hostname
# that answered nowhere, so a mismatch must change nothing at all.
reset_move_config
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf 'INF Added CNAME relay-fedora.new.test.old.test which will route to your tunnel\n'
exit 0
EOF
chmod 700 "$MOVE_BIN/cloudflared"
MOVE_OUTPUT="$(
    printf 'relay-fedora.new.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"created relay-fedora.new.test.old.test, not relay-fedora.new.test"*) ;;
    *)
        echo "a mis-zoned route was accepted" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
grep -Fq 'hostname: relay-fedora.new.test' "$MOVE_CONFIG" &&
    { echo "a mis-zoned route still rewrote the ingress" >&2; exit 1; }

# A new name the edge never answers must not take the old one out of service.
reset_move_config
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf 'INF Added CNAME relay-fedora.unreachable.test which will route to your tunnel\n'
exit 0
EOF
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
exit 6
EOF
chmod 700 "$MOVE_BIN/cloudflared" "$MOVE_BIN/curl"
MOVE_OUTPUT="$(
    printf 'relay-fedora.unreachable.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        HERDR_STABLE_DNS_TIMEOUT=0 \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"does not resolve to Cloudflare yet"*) ;;
    *)
        echo "an unresolvable new hostname was accepted" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
grep -Fq 'hostname: relay-fedora.old.test' "$MOVE_CONFIG" ||
    { echo "the old hostname was lost while the new one was unreachable" >&2; exit 1; }

# The certificate names the only zone cloudflared can route into. When the new
# hostname is outside it, the refusal has to come before any record exists.
reset_move_config
: > "$MOVE_ROUTE_LOG"
MOVE_CERT="$MOVE_HOME/cert.pem"
{
    printf -- '-----BEGIN ARGO TUNNEL TOKEN-----\n'
    printf '{"zoneID":"zone1","apiToken":"token1","accountID":"acct1"}' | base64
    printf -- '-----END ARGO TUNNEL TOKEN-----\n'
} > "$MOVE_CERT"
cat > "$MOVE_BIN/curl" <<'EOF'
#!/bin/sh
case "$*" in
    *"/zones/zone1"*)
        printf '{"result":{"id":"zone1","name":"old.test"},"success":true}\n'
        ;;
    *) printf '{"status":"ok","instance":"same","version":"9.9.9","protocol":2}\n' ;;
esac
EOF
cat > "$MOVE_BIN/cloudflared" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$MOVE_ROUTE_LOG"
exit 0
EOF
chmod 700 "$MOVE_BIN/curl" "$MOVE_BIN/cloudflared"
MOVE_OUTPUT="$(
    printf 'relay-fedora.elsewhere.test\n' |
        HOME="$MOVE_HOME" PATH="$MOVE_BIN:$PATH" \
        HERDR_RELAY_BIN="$MOVE_BIN/relay-stub" HERDR_RELAY_ENV="$MOVE_ENV" \
        TUNNEL_ORIGIN_CERT="$MOVE_CERT" HERDR_STABLE_RELOGIN=false \
        bash "$REPO_DIR/relay/change-hostname.sh" 2>&1 || true
)"
case "$MOVE_OUTPUT" in
    *"authorizes old.test"*"cloudflared tunnel login"*) ;;
    *)
        echo "a hostname outside the certificate's zone was not refused" >&2
        printf '%s\n' "$MOVE_OUTPUT" >&2
        exit 1
        ;;
esac
[ ! -s "$MOVE_ROUTE_LOG" ] ||
    { echo "a record was created before the zone was checked" >&2; exit 1; }
grep -Fq 'hostname: relay-fedora.old.test' "$MOVE_CONFIG" ||
    { echo "the refused move still touched the ingress" >&2; exit 1; }

echo "change-hostname shell tests passed"
