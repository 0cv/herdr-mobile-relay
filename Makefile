ifneq (,$(wildcard .env))
include .env
export
endif

WEB_PROJECT ?= herdr-mobile-relay
PATH := /opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$(HOME)/.local/bin:$(PATH)
export PATH

.PHONY: help setup setup-link rotate-token quick-start dev-tunnel stable-setup stable-teardown check backend-check frontend-check frontend-browser frontend-browser-release test relay-run relay-plugin service-install service-uninstall service-status service-logs macos-service-install macos-service-uninstall macos-service-status macos-service-logs linux-service-install linux-service-uninstall linux-service-status linux-service-logs web-bundle-check web-release web-release-check web-deploy web-preview

help:
	@echo "Common targets:"
	@echo "  make quick-start                First run: install missing tools and start the phone app"
	@echo "  make dev-tunnel                Build and tunnel an isolated frontend for development"
	@echo "  make stable-setup               Provision/resume a stable tunnel, service, and verified QR"
	@echo "  make stable-teardown            Remove only resources recorded by the stable wizard"
	@echo "  make setup                      Prepare config and check prerequisites without installing"
	@echo "  make web-deploy                 Deploy ./web to Cloudflare Pages (WEB_PROJECT=$(WEB_PROJECT))"
	@echo "  make web-release                Replace ./web with a verified frontend release build"
	@echo "  make service-install            Install/start the relay service for this platform"
	@echo "  make setup-link                 Print the phone setup link and QR code for a stable relay"
	@echo "    APP_URL=app.example.com       One-time installed-PWA origin override"
	@echo "  make rotate-token               Replace the relay token and print a new setup link"
	@echo "  make service-status             Show relay service status"
	@echo "  make service-logs               Tail relay service logs"
	@echo "  make service-uninstall          Stop/remove the relay service"
	@echo "  make relay-run                  Run relay in the foreground"
	@echo "  make check                      Run backend and frontend checks"

setup:
	relay/setup.sh

setup-link:
	HERDR_PHONE_APP_URL="$(APP_URL)" relay/setup-link.sh $(HOST)

rotate-token:
	relay/rotate-token.sh

quick-start:
	relay/setup.sh --install-missing
	relay/start.sh

dev-tunnel:
	relay/dev-tunnel.sh

stable-setup:
	relay/stable-setup.sh

stable-teardown:
	relay/stable-teardown.sh

check: backend-check frontend-check

backend-check: test
	uv run --with ruff ruff check relay tests
	uv run python -m compileall -q relay tests
	@for script in relay/*.sh; do bash -n "$$script" || exit; done
	@for script in relay/plugin-build.sh relay/plugin-on-event.sh relay/plugin-post-install.sh; do sh -n "$$script" || exit; done
	bash -n relay/plugin-setup-terminal.command
	tests/test_stable_setup.sh

frontend-check:
	npm --prefix frontend run lint
	npm --prefix frontend run check
	npm --prefix frontend run test
	npm --prefix frontend run build
	npm --prefix frontend run size
	node --check frontend/public/sw.js
	node --check frontend/public/notification-icons.js
	bash -n frontend/scripts/run-browser-tests.sh

frontend-browser:
	frontend/scripts/run-browser-tests.sh dist

frontend-browser-release:
	frontend/scripts/run-browser-tests.sh ../web

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

web-bundle-check:
	node frontend/scripts/validate-build.mjs web
	node frontend/scripts/check-size.mjs web
	node --check web/sw.js
	node --check web/notification-icons.js

web-release: frontend-check
	node frontend/scripts/release.mjs
	$(MAKE) web-bundle-check

web-release-check: web-bundle-check frontend-browser-release

web-deploy: web-bundle-check
	npx wrangler pages deploy web --project-name "$(WEB_PROJECT)"

web-preview:
	npx wrangler pages dev web
