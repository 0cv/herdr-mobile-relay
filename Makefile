ifneq (,$(wildcard .env))
include .env
export
endif

WEB_PROJECT ?= herdr-mobile-relay
WEB_BRANCH ?= main
WRANGLER_VERSION ?= 4.112.0
PATH := /opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$(HOME)/.local/bin:$(PATH)
export PATH

.PHONY: help setup setup-link app-deploy-setup rotate-token quick-start dev-tunnel stable-setup stable-teardown check go-check backend-check shell-check production-path-audit cross-build release-bundle-check frontend-check frontend-browser frontend-browser-release test relay-run relay-plugin service-install service-uninstall service-status service-logs macos-service-install macos-service-uninstall macos-service-status macos-service-logs linux-service-install linux-service-uninstall linux-service-status linux-service-logs web-bundle-check web-release web-release-check web-deploy web-preview

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
	@echo "  make app-deploy-setup           Authorize this relay to deploy a separate Pages app"
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

app-deploy-setup:
	relay/configure-app-deploy.sh

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

check: backend-check frontend-check frontend-browser web-release-check cross-build release-bundle-check

go-check:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './frontend/node_modules/*'))"
	go vet ./...
	go test ./...
	go test -race ./...

backend-check: go-check shell-check production-path-audit

shell-check:
	@for script in relay/*.sh; do bash -n "$$script" || exit; done
	@for script in relay/plugin-on-event.sh; do sh -n "$$script" || exit; done
	@for script in install.sh scripts/*.sh; do sh -n "$$script" || exit; done
	bash -n relay/plugin-setup-terminal.command
	sh tests/test_install.sh
	bash tests/test_common.sh
	bash tests/test_plugin_build.sh
	sh tests/test_release_scripts.sh
	bash tests/test_uninstall.sh
	tests/test_stable_setup.sh

production-path-audit:
	@if rg -n '(^|[;&|][[:space:]]*)(python3?|uv)([[:space:]]|$$)' relay --glob '*.sh' --glob '*.command'; then \
		echo "Production shell path still invokes Python or uv" >&2; \
		exit 1; \
	fi
	@if rg -n '(^|[;&|][[:space:]]*)go[[:space:]]+(build|run|install)' relay install.sh; then \
		echo "End-user install/runtime path still invokes the Go toolchain" >&2; \
		exit 1; \
	fi

cross-build:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		os="$${target%/*}"; arch="$${target#*/}"; \
		CGO_ENABLED=0 GOOS="$$os" GOARCH="$$arch" go build -trimpath \
			-o "$$tmp/herdr-mobile-relay-$$os-$$arch" ./cmd/herdr-mobile-relay || exit; \
	done

release-bundle-check:
	@tmp="$$(mktemp -d)"; trap 'rm -rf "$$tmp"' EXIT; \
	version="$$(sed -n 's/^version = "\([^"]*\)"/\1/p' herdr-plugin.toml)"; \
	revision="$$(git rev-parse HEAD 2>/dev/null || echo test-revision)"; \
	scripts/package-release.sh "$$version" "$$revision" "$$tmp"; \
	test "$$(find "$$tmp" -name 'herdr-mobile-relay_*.tar.gz' | wc -l)" -eq 4; \
	test -s "$$tmp/checksums.txt"; \
	host_os="$$(go env GOOS)"; host_arch="$$(go env GOARCH)"; \
	scripts/check-installed-release.sh \
		"$$tmp/herdr-mobile-relay_$${version}_$${host_os}_$${host_arch}.tar.gz" \
		"$$tmp/checksums.txt" "$$version" "$$revision" "$${host_os}/$${host_arch}"

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
	go test ./...

relay-run:
	go run ./cmd/herdr-mobile-relay serve

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

web-release:
	node frontend/scripts/bump-assets.mjs
	$(MAKE) frontend-check
	node frontend/scripts/release.mjs
	$(MAKE) web-bundle-check

web-release-check: web-bundle-check
	@diff -qr frontend/dist web
	$(MAKE) frontend-browser-release

web-deploy: web-bundle-check
	npx --yes wrangler@$(WRANGLER_VERSION) pages deploy web --project-name "$(WEB_PROJECT)" --branch "$(WEB_BRANCH)"

web-preview:
	npx wrangler pages dev web
