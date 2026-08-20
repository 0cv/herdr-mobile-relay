# Local development

How to build, run, and test this project from a checkout, and how to fix the
local runtime problems that come up while doing it. Read this if you are changing
the relay rather than using it.

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

## Testing a release candidate

Candidates are published as prereleases, which ordinary relays never install:
their update check resolves the latest stable release only. To run one:

```bash
herdr plugin install 0cv/herdr-mobile-relay --ref dev
```

Rerun that command to move to a newer candidate.

## Contributing

Work lands on `dev`; open pull requests against it and make sure `make check`
passes first.

## Toolchains

Backend development uses Go 1.27.0; frontend development uses Node.js 26.
Packaged users need neither toolchain.

The test-only `cmd/fake-herdr` binary provides deterministic Herdr CLI behavior,
failure injection, and process-control traces for black-box tests.

## Troubleshooting local runs

- **Port 8375 is busy:** stop the earlier Quick Start or installed service.
- **Herdr is not running:** start it with `herdr`, then retry the operation.
- **Agents are unavailable:** inspect `/healthz`; after a Herdr protocol update,
  run `herdr server live-handoff` and wait for the next relay poll.
