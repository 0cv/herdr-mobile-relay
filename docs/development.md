# Local development

How to build and run this checkout from source, which make targets matter, and
how to fix the local runtime problems that come up while doing it. Read this if
you are changing the relay, not just using it.

## Running from a checkout

```bash
git clone git@github.com:0cv/herdr-mobile-relay-dev.git
cd herdr-mobile-relay-dev
make dev-tunnel
```

`make dev-tunnel` builds the current Go source and frontend, uses isolated ports
and state under `relay/.dev/`, and opens a temporary tunnel. It never uses the
installed production relay.

## Common targets

```bash
make check             # all backend, frontend, browser, and release checks
make backend-check     # format, vet, tests, race detector, shell checks
make web-release       # replace committed web/ with a verified frontend build
make web-release-check # compare and browser-test the shipped web/ bundle
make relay-plugin      # link this checkout as a Herdr plugin
make stable-setup      # install a checkout-managed stable relay
```

`make check` is the full gate: it runs the backend, frontend, browser, and
release checks. `make backend-check` covers the Go side on its own — format,
vet, tests, the race detector, and shell checks. The frontend build shipped in
`web/` is produced by `make web-release` and verified against the committed
bundle by `make web-release-check`, which also browser-tests it.

## Installing a private canary

`herdr plugin install` clones the named repository, and the plugin then
installs the verified release of that same repository. A private repository's
assets need a GitHub token with read access to it:

```bash
GH_TOKEN=<token> herdr plugin install 0cv/herdr-mobile-relay-dev
```

The token is stored as `github-token` beside `relay.env`, readable only by the
installing user; the relay, `cloudflared`, and agent subprocesses never inherit
it. Managed self-updates still track this project's public releases, so a
canary newer than the public tag reports that it is up to date.

## Toolchains

Backend development uses Go 1.26.5; frontend development uses Node.js 24.
Packaged users need neither toolchain.

The test-only `cmd/fake-herdr` binary provides deterministic Herdr CLI behavior,
failure injection, and process-control traces for black-box tests. It is not
included in release archives.

## Troubleshooting local runs

- **Port 8375 is busy:** stop the earlier Quick Start or installed service.
- **Herdr is not running:** start it with `herdr`, then retry the operation.
- **Agents are unavailable:** inspect `/healthz`; after a Herdr protocol update,
  run `herdr server live-handoff` and wait for the next relay poll.
