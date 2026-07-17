#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "🐑 Herdr Mobile Relay Setup"
echo ""
echo "Choose how you want to start:"
echo ""
echo "  1. Quick Start (recommended)"
echo "     Temporary TryCloudflare URL; no account or domain required."
echo ""
echo "  2. Stable Tunnel"
echo "     Guided permanent hostname, dedicated tunnel, and background service."
echo ""
echo "  3. Show Phone Setup QR"
echo "     Reprint the private link and QR for the installed stable relay."
echo ""
echo "  4. Remove Stable Tunnel"
echo "     Tear down only resources recorded as wizard-owned."
echo ""
echo "  q. Exit"
echo ""

while true; do
    read -r -p "Choice [1]: " choice
    case "${choice:-1}" in
        1)
            exec "$SCRIPT_DIR/plugin-quick-start.sh"
            ;;
        2)
            exec "$SCRIPT_DIR/plugin-install-service.sh"
            ;;
        3)
            exec "$SCRIPT_DIR/plugin-setup-link.sh"
            ;;
        4)
            exec "$SCRIPT_DIR/plugin-stable-teardown.sh"
            ;;
        q|Q)
            exit 0
            ;;
        *)
            echo "Enter 1, 2, 3, 4, or q."
            ;;
    esac
done
