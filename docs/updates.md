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

## The deployment-owner role

The relay-hosted app updates with its relay. For a separately hosted Cloudflare
Pages app, configure exactly one stable relay as deployment owner with the
`configure-app-deploy` action. The worker deploys and verifies the target app
before it installs and restarts the relay; a failed download, compatibility
check, deployment, or public-origin check leaves the current relay running.

The optional deployment-owner role requires Node.js 24 and Cloudflare
credentials on that computer only. The action looks for `node` where nvm, fnm,
volta, and asdf put it, not only on `PATH`.

A Pages project can only deploy a domain it already serves. When the origin you
enter is not one of them, the action attaches it for you if `relay.env` carries
a `CLOUDFLARE_API_TOKEN` with Pages:Edit — the `wrangler login` alone cannot,
since Wrangler 4 has no custom-domain command. Set
`HERDR_APP_DEPLOY_ATTACH_DOMAIN=true` to allow that without being asked, or
`false` to refuse. Without a token it names the credential to set and offers to
take a different origin.

## Release checks and app reloads

Release checks use the GitHub API and fall back to the public GitHub Atom release
and commit feeds when an unauthenticated API request is rate-limited. Loading a
newly deployed phone app uses a versioned navigation, so a sleeping browser or
installed PWA does not reuse a stale document.

## Herdr version compatibility

The relay continues to support Herdr 0.7.5 or newer.

Herdr 0.8.0 and newer can resume restored agent sessions without a TUI
attached ([#2064](https://github.com/herdrdev/herdr/issues/2064)) and keep the
desktop user's focus when a background workspace closes
([#1328](https://github.com/herdrdev/herdr/issues/1328),
[#1621](https://github.com/herdrdev/herdr/issues/1621)). Phone-driven **Stop**
still cascades a single-tab workspace away; the workspace then reports
`workspace_not_found`.

## Troubleshooting

- **Update operation failed with `read canonical release: HTTP 403`:** an older
  relay's unauthenticated GitHub release check was rate-limited. Run
  `HERDR_MOBILE_RELAY_NO_AUTO_SETUP=1 herdr plugin install 0cv/herdr-mobile-relay --yes`
  once on that computer as the signed-in user; current releases retry through
  GitHub's public release feeds.
- **Updated app still shows the previous version:** open Settings, choose
  **Check for Updates**, then **Load Update**.
