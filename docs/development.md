# Local development

How to build and run this checkout from source, which make targets matter, and
how to fix the local runtime problems that come up while doing it. Read this if
you are changing the relay, not just using it.

## Running from a checkout

```bash
git clone https://github.com/0cv/herdr-mobile-relay.git
cd herdr-mobile-relay
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

## Shipping a release candidate

Work lands on `dev` with no tag; the check workflow runs on that branch, so a
candidate is gated before it is ever tagged. Tag `vX.Y.Z` on `dev` only when
testers have to install it. The release workflow publishes that tag as a
prerelease, which ordinary relays never see: their update check resolves the
latest stable release only.

```bash
herdr plugin install 0cv/herdr-mobile-relay --ref dev
```

Testers move to a newer candidate by re-running that same command.

Promote a candidate by merging `dev` into `main` with a merge commit or a
fast-forward — never a squash, because `install.sh` pins the installed manifest
revision to the tag's commit and the published tag has to stay an ancestor of
`main`. Then flip the release:

```bash
gh release edit vX.Y.Z --prerelease=false --latest
```

A published tag is never moved. Withdraw a bad candidate with
`gh release delete vX.Y.Z --cleanup-tag` before anyone installs it, then re-tag
the same version from the fixed commit.

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
