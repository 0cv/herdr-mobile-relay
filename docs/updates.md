# Updates and Herdr compatibility

How relay releases are verified, activated, and rolled back, how phone-driven
upgrades work, and which Herdr versions the relay supports. Read this before
upgrading a relay or hosting the phone app separately.

## How a release is installed

The plugin installs a pre-built, checksum- and manifest-verified bundle for the
exact version in `herdr-plugin.toml`. Users never compile the relay. Updates
atomically activate the executable, web app, and runtime wrappers, verify their
exact version, revision, and web hash after restart, and roll back the complete
release if verification fails.

Phone-driven upgrades run `herdr plugin install` in a transient worker pinned
to the release commit. The same plugin build hook can restore stale service
paths from the persistent plugin config, including when no usable local release
remains.

## The deployment-owner role

The relay-hosted app updates with its relay. For a separately hosted Cloudflare
Pages app, configure exactly one stable relay as deployment owner with the
`configure-app-deploy` action. From relays running this release onward, the
worker downloads and verifies the target release without activating it, checks
that the current and target apps and relays share a transport, deploys the
target web bundle, and verifies the public Pages version. Only then does it
install and restart the relay. A failed download, compatibility check,
deployment, or public-origin check leaves the current relay running.

The optional deployment-owner role requires Node.js 24 and Cloudflare
credentials on that computer only.

## Release checks and app reloads

Release checks use the GitHub API and fall back to the public GitHub Atom
release and commit feeds when an unauthenticated API request is rate-limited.
Loading a newly deployed phone app uses a versioned navigation, so a sleeping
browser or installed PWA does not have to reuse a stale document.
Transport-breaking changes require a bridge release that supports both
transports. This release retains the existing E2EE v1 transport, so the upgrade
into it remains compatible with the previous phone app.

## Herdr version compatibility

The relay continues to support Herdr 0.7.5 or newer.

Herdr 0.8.0 and newer can resume restored agent sessions without a TUI
attached ([#2064](https://github.com/herdrdev/herdr/issues/2064)) and keep the
desktop user's focus when a background workspace closes
([#1328](https://github.com/herdrdev/herdr/issues/1328),
[#1621](https://github.com/herdrdev/herdr/issues/1621)). These upstream
behaviors keep relay startup and phone-driven workspace management
non-disruptive. Phone-driven **Stop** still cascades a single-tab workspace
away; the workspace then reports `workspace_not_found`.

## Troubleshooting

- **Update operation failed with `read canonical release: HTTP 403`:** an older
  relay's unauthenticated GitHub release check was rate-limited. Run
  `HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install 0cv/herdr-mobile-relay --yes`
  once on that computer as the signed-in user; current releases retry through
  GitHub's public release feeds.
- **Updated app still shows the previous version:** open Settings, choose
  **Check for Updates**, then **Load Update**. Current releases use a fresh
  versioned navigation and preserve the saved relay list.
