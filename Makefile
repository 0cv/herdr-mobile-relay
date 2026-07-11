ifneq (,$(wildcard .env))
include .env
export
endif

WEB_PROJECT ?= herdr-mobile-relay
PATH := /opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$(HOME)/.local/bin:$(PATH)
export PATH

.PHONY: help setup setup-link rotate-token quick-start check test relay-run relay-plugin service-install service-uninstall service-status service-logs macos-service-install macos-service-uninstall macos-service-status macos-service-logs linux-service-install linux-service-uninstall linux-service-status linux-service-logs web-deploy web-preview

help:
	@echo "Common targets:"
	@echo "  make quick-start                First run: install missing tools and start the phone app"
	@echo "  make setup                      Prepare config and check prerequisites without installing"
	@echo "  make web-deploy                 Deploy ./web to Cloudflare Pages (WEB_PROJECT=$(WEB_PROJECT))"
	@echo "  make service-install            Install/start the relay service for this platform"
	@echo "  make setup-link                 Print the phone setup link and QR code for a stable relay"
	@echo "  make rotate-token               Replace the relay token and print a new setup link"
	@echo "  make service-status             Show relay service status"
	@echo "  make service-logs               Tail relay service logs"
	@echo "  make service-uninstall          Stop/remove the relay service"
	@echo "  make relay-run                  Run relay in the foreground"
	@echo "  make check                      Run lint, syntax checks, and tests"

setup:
	relay/setup.sh

setup-link:
	relay/setup-link.sh $(HOST)

rotate-token:
	relay/rotate-token.sh

quick-start:
	relay/setup.sh --install-missing
	relay/start.sh

check: test
	uv run --with ruff ruff check relay tests
	uv run python -m compileall -q relay tests
	@for script in relay/*.sh; do bash -n "$$script" || exit; done
	node --check web/sw.js
	node --check web/notification-icons.js
	node -e 'const fs=require("fs");const html=fs.readFileSync("web/index.html","utf8");const start=html.lastIndexOf("<script>");const end=html.indexOf("</script>",start);if(start<0||end<0)throw new Error("inline script not found");new Function(html.slice(start+8,end));'
	node tests/test_web_terminal.js
	node tests/test_web_activity.js
	node tests/test_web_launch.js
	node tests/test_web_protocol.js

test:
	uv run --with 'websockets>=14.0' --with 'pywebpush>=2.0.0' --with 'py-vapid>=1.9.2' --with 'cryptography>=42.0.0' python -m unittest discover -s tests -v

relay-run:
	uv run relay/herdr_relay.py

relay-plugin:
	herdr plugin link .

service-install:
	relay/service.sh install

service-uninstall:
	relay/service.sh uninstall

service-status:
	relay/service.sh status

service-logs:
	relay/service.sh logs

macos-service-install:
	relay/install-service.sh

macos-service-uninstall:
	relay/uninstall-service.sh

macos-service-status:
	launchctl print gui/$$(id -u)/com.herdr-mobile-relay.service

macos-service-logs:
	tail -f "$$HOME/Library/Logs/herdr-mobile-relay/service.log" "$$HOME/Library/Logs/herdr-mobile-relay/service.err"

linux-service-install:
	relay/install-systemd-user-service.sh

linux-service-uninstall:
	relay/uninstall-systemd-user-service.sh

linux-service-status:
	systemctl --user status herdr-mobile-relay.service

linux-service-logs:
	journalctl --user -u herdr-mobile-relay.service -f

web-deploy:
	npx wrangler pages deploy web --project-name "$(WEB_PROJECT)"

web-preview:
	npx wrangler pages dev web
