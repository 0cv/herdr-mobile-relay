# Updates and Herdr compatibility

How relay releases are verified, activated, and rolled back, how phone-driven
upgrades work, and which Herdr versions the relay supports. Read this before
upgrading a relay or hosting the phone app separately.

## How a release is installed

The plugin installs a pre-built, checksum- and manifest-verified bundle for the
exact version in `herdr-plugin.toml`. Updates atomically activate the
executable, web app, and runtime wrappers, verify their exact version,
revision, and web hash after restart, and roll back the complete release if
verification fails.

Phone-driven upgrades run `herdr plugin install` in a transient worker pinned
to the release commit.

## Upgrading from v0.19.1

Version 0.20.0 replaces E2EE v1 and the shared relay key with E2EE v2 and
per-device credentials. The phone updater intentionally refuses this transport
boundary because neither an app-first nor a relay-first rollout can keep the
old phone connected.

Upgrade each relay manually, exactly as for a fresh install:

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

The setup menu opens by itself a moment after the install finishes. If it does
not, open it:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Choose your connection option again to print a fresh bootstrap QR. It is
one-use: pair exactly one phone with it, making that phone the first
controller. Use **Settings → Devices → Invite Device** on that controller to
create a separate invitation for every additional controller or reader.
Previously paired phones cannot reuse their v0.19.1 key or the already-consumed
bootstrap link.

`HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1` in front of the install command suppresses
the automatic menu; it exists for unattended upgrades, not for this one.

## The deployment-owner role

The relay-hosted app updates with its relay. For a separately hosted Cloudflare
Pages app, configure exactly one stable relay as deployment owner with the
`configure-app-deploy` action. The worker deploys and verifies the target app
before it installs and restarts the relay; a failed download, compatibility
check, deployment, or public-origin check leaves the current relay running.

The optional deployment-owner role requires Node.js 22 or newer — Wrangler's own
floor, and what the action verifies before it records a directory — plus
Cloudflare credentials on that computer only. The action looks for `node` where
nvm, fnm, volta, and asdf put it, not only on `PATH`.

A Pages project can only deploy a domain it already serves. When the origin you
enter is not one of them, and the account has exactly one Pages project, the
action attaches it for you if `relay.env` carries a `CLOUDFLARE_API_TOKEN` with
Pages:Edit — the updater pins Wrangler 4.125.0 and skips Wrangler's differential
asset cache during deployment because that upload path can fail against stale
Pages cache state. Set `HERDR_APP_DEPLOY_ATTACH_DOMAIN=true` to allow that
without being asked, or `false` to refuse. Otherwise it names the credential to
set and offers to take a different origin.

## Release checks and app reloads

Release checks use the GitHub API. When an unauthenticated request is rate
limited, they fall back to the public `releases/latest` redirect for the stable
tag and that tag's Atom commit feed for its revision. Loading a
newly deployed phone app uses a versioned navigation, so a sleeping browser or
installed PWA does not reuse a stale document.

## Herdr version compatibility

The relay continues to support Herdr 0.7.5 or newer.

Herdr 0.8.0 and newer can resume restored agent sessions without a TUI
attached ([#2064](https://github.com/herdrdev/herdr/issues/2064)) and keep the
desktop user's focus when a background workspace closes
([#1328](https://github.com/herdrdev/herdr/discussions/1328),
[#1621](https://github.com/herdrdev/herdr/issues/1621)). Phone-driven **Stop**
still cascades a single-tab workspace away; the workspace then reports
`workspace_not_found`.

Herdr releases newer than 0.8.2 add screen-based working-state fallbacks for
Claude Code ([#1630](https://github.com/herdrdev/herdr/issues/1630),
[#2241](https://github.com/herdrdev/herdr/issues/2241)): a visible turn,
background shells, or background agents keep a pane reported as working when
terminal titles are unavailable or disabled. That accuracy flows straight to
the phone, which keys completion notifications and history capture off those
status transitions.

## Troubleshooting

- **Update operation failed with `read canonical release: HTTP 403`:** an older
  relay's unauthenticated GitHub release check was rate-limited. Run
  `HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install 0cv/herdr-mobile-relay --yes`
  once on that computer as the signed-in user; current releases retry through
  the public release redirect and commit feed.
- **Updated app still shows the previous version:** open Settings, choose
  **Check for Updates**, then **Load Update**.
