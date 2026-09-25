#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LAUNCHER="$ROOT/relay/tailscale.sh"
REPRINT="$ROOT/relay/setup-link.sh"
GO_MAIN="$ROOT/cmd/herdr-mobile-relay/main.go"
ROUTE_CONTRACT="$ROOT/internal/tailscale/contract.go"
HUB="$ROOT/internal/transport/ws.go"
COMMON="$ROOT/relay/common.sh"

require_text() {
    local file="$1" text="$2"
    grep -F -- "$text" "$file" >/dev/null || {
        echo "FAIL $file is missing required Tailscale contract: $text" >&2
        exit 1
    }
}
forbid_text() {
    local file="$1" text="$2"
    if grep -F -- "$text" "$file" >/dev/null; then
        echo "FAIL $file contains forbidden Tailscale ownership shortcut: $text" >&2
        exit 1
    fi
}

# Keep the shell launcher and its reprint path aligned with the single in-process
# SessionAuthority. This is a source-contract check, not daemon/package evidence.
for text in \
    'SECOND_INSPECTION=' \
    'local_ready' \
    'owner_held' \
    'activation-pending' \
    'arm-pending' \
    'arm_bootstrap' \
    'route_cleared' \
    'local_watch_closed' \
    'tailscale-route-check'; do
    require_text "$LAUNCHER" "$text"
done
require_text "$REPRINT" 'SESSION_STAGE" = ready'
require_text "$REPRINT" 'tailscale-route-check'
require_text "$LAUNCHER" 'verify_phone_app_bundle'
require_text "$REPRINT" 'verify_phone_app_bundle'
require_text "$COMMON" 'verify-public --web-root'
require_text "$GO_MAIN" 'tailscale-route-check'
require_text "$GO_MAIN" 'ManagedRouteMatches'
require_text "$ROUTE_CONTRACT" 'route.Session != ""'
require_text "$HUB" 'admissionRevoked'
require_text "$HUB" 'func (h *Hub) RevokeAdmission()'

for file in "$LAUNCHER" "$REPRINT"; do
    forbid_text "$file" 'serve_route_owned'
    forbid_text "$file" 'SERVE_PID'
    forbid_text "$file" 'SERVE_JOB'
    forbid_text "$file" 'serve off'
    forbid_text "$file" 'HERDR_TAILSCALE_CA_FILE'
done
if grep -E '\$TS_BIN[[:space:]]+serve([[:space:]]|$)' "$LAUNCHER" >/dev/null; then
    echo "FAIL managed launcher invokes Tailscale Serve through the CLI" >&2
    exit 1
fi

for script in "$ROOT"/relay/*.sh; do
    bash -n "$script"
done

if [ "${HERDR_TAILSCALE_LAUNCHER_CI:-}" = 1 ]; then
    python3 -B "$ROOT/tests/test_tailscale_launcher_lifecycle.py" -v
else
    printf 'SKIP hosted-only Tailscale launcher lifecycle fixture (required hosted check enables it)\n'
fi

printf 'PASS Tailscale source contract and shell syntax\n'
