#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "🐑 Herdr Mobile Relay Setup"
echo ""
echo "Choose how you want to start:"
echo ""
echo "  1. Choose Connection Method"
echo "     Decide how your phone reaches this computer: the community gateway"
echo "     (free, shared, run by the project), your own gateway (best"
echo "     performance, you own the bandwidth and the logs), or a Cloudflare"
echo "     tunnel."
echo ""
echo "  2. Quick Start (recommended)"
echo "     Installs missing tools, starts the relay, and prints the phone setup"
echo "     QR code using whichever connection method is configured."
echo ""
echo "  3. Stable Tunnel"
echo "     Guided permanent Cloudflare hostname, dedicated tunnel, and background service."
echo ""
echo "  4. Show Phone Setup QR"
echo "     Reprint the private link and QR for the installed relay, tunnel or gateway."
echo ""
echo "  5. Remove Stable Tunnel"
echo "     Tear down only resources recorded as wizard-owned."
echo ""
echo "  6. Configure App Deployment"
echo "     Let this computer deploy one separately hosted Cloudflare Pages app."
echo ""
echo "  q. Exit"
echo ""

while true; do
    read -r -p "Choice [1]: " choice
    case "${choice:-1}" in
        1)
            exec "$SCRIPT_DIR/plugin-choose-transport.sh"
            ;;
        2)
            exec "$SCRIPT_DIR/plugin-quick-start.sh"
            ;;
        3)
            exec "$SCRIPT_DIR/plugin-install-service.sh"
            ;;
        4)
            exec "$SCRIPT_DIR/plugin-setup-link.sh"
            ;;
        5)
            exec "$SCRIPT_DIR/plugin-stable-teardown.sh"
            ;;
        6)
            exec "$SCRIPT_DIR/plugin-configure-app-deploy.sh"
            ;;
        q|Q)
            exit 0
            ;;
        *)
            echo "Enter 1, 2, 3, 4, 5, 6, or q."
            ;;
    esac
done
