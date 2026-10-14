#!/usr/bin/env bash
set +x
set -euo pipefail

[ "$#" -le 1 ] || { echo 'Invalid service-removal arguments.' >&2; exit 2; }
ROUTE_DISPOSITION="${1:-}"
case "$ROUTE_DISPOSITION" in
    '') [ "$#" -eq 0 ] || { echo 'Invalid service-removal arguments.' >&2; exit 2; } ;;
    --cli-route-disposition=retained) ROUTE_DISPOSITION=retained ;;
    --cli-route-disposition=removed) ROUTE_DISPOSITION=removed ;;
    *) echo 'Invalid service-removal arguments.' >&2; exit 2 ;;
esac
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"
require_user_service_context || exit 1
case "$(uname -s)" in Darwin) ;; *) echo 'Unsupported native service-removal platform.' >&2; exit 1 ;; esac
trap 'exit 130' INT
trap 'exit 143' TERM
ENV_FILE="$(service_environment_file "$SCRIPT_DIR")" || { echo 'Service-removal environment selection is unavailable; no native service action started.' >&2; exit 1; }
service_cli_route_disposition "$ENV_FILE" "$SCRIPT_DIR" "$ROUTE_DISPOSITION" || exit 1
if service_removal_all launchd; then exit 0; else exit 1; fi
