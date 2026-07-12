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
echo "  2. Stable Tunnel (advanced)"
echo "     Permanent hostname and background service; requires an existing"
echo "     named Cloudflare tunnel configuration."
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
        q|Q)
            exit 0
            ;;
        *)
            echo "Enter 1, 2, or q."
            ;;
    esac
done
