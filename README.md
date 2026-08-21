# Herdr Mobile Relay

[![check](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml/badge.svg)](https://github.com/0cv/herdr-mobile-relay/actions/workflows/check.yml)

Control [Herdr](https://herdr.dev) agents from your phone. Each Linux or macOS
computer runs its own relay; the phone connects to them and merges every agent
into one installable web app.

**Current version:** [`0.17.2`](https://github.com/0cv/herdr-mobile-relay/releases/tag/v0.17.2) · [Changelog](CHANGELOG.md)

> [!IMPORTANT]
> Native Windows is not supported. WSL2 may work but is not tested.

## Get started in two minutes

Requirements: Herdr 0.7.5 or newer, Git, and `curl`.

```bash
herdr plugin install 0cv/herdr-mobile-relay
```

Choose **Temporary Cloudflare Tunnel** from the setup menu. It installs any
missing user-level tools with confirmation, starts the relay and bundled app,
and prints a QR code. If the menu does not open:

```bash
herdr plugin action invoke setup --plugin herdr-mobile-relay.events
```

Scan the QR with your phone. Keep the pane open; Ctrl-C stops the temporary
relay.

No Cloudflare account, no domain, no `sudo`, and no Python, Node.js, or Go
toolchain. Treat the QR and its setup link as secrets: they carry the relay key.

[QUICKSTART.md](QUICKSTART.md) is the same path with pairing detail and
troubleshooting.

## What you get

| | |
| --- | --- |
| <img src="images/home.jpeg" alt="Mobile list of Herdr agents" width="392"> | <img src="images/agent_plan.jpeg" alt="Structured plan question navigation" width="392"> |

- Monitor and control agents across several computers, grouped by status and
  workspace, with agents that need input pinned on top.
- Start, rename, clear, and stop agents; send prompts, terminal keys, slash
  commands, screenshots, and photos.
- Answer verified approvals and structured plan questions from Codex, Claude
  Code, Qoder, OpenCode, Oh My Pi, and Pi.
- Read searchable native conversation history, and inspect workspace files,
  images, and Git diffs read-only.
- Receive blocked-agent notifications, with completion notifications optional.

**[Full feature tour →](docs/mobile-app.md)**

## Mobile onboarding

https://github.com/user-attachments/assets/e52c4fd0-ef77-4852-bb43-078a7154eae8

The walkthrough follows setup from scanning the QR through the agent list,
terminal controls, and notification settings.

## Choosing how your phone connects

The setup menu exposes each complete connection path directly:

| Choice | Needs | Best for |
| --- | --- | --- |
| Cloudflare tunnel | nothing, or a domain for the stable variant | the default; a permanent hostname with a background service |
| Community gateway | no account, no domain, but an app origin to pair against | free, shared, best-effort |
| Your own gateway | a small VPS | dedicated bandwidth and control of the transport logs |

All three are end-to-end encrypted. On either gateway the phone and the computer
then negotiate a direct peer-to-peer connection, leaving the gateway with the
fallback; Cloudflare tunnel traffic stays on Cloudflare.

- **[Transports explained →](docs/transports.md)**
- **[Permanent Cloudflare tunnel →](docs/cloudflare-tunnel.md)**
- **[Run your own gateway →](docs/gateway-self-hosting.md)**

## Documentation

| Page | What is in it |
| --- | --- |
| [QUICKSTART.md](QUICKSTART.md) | The fast path, start to paired phone |
| [docs/mobile-app.md](docs/mobile-app.md) | Every feature: agent list, workspace inspection, mobile terminal |
| [docs/transports.md](docs/transports.md) | Cloudflare, community gateway, own gateway, direct WebRTC |
| [docs/cloudflare-tunnel.md](docs/cloudflare-tunnel.md) | The stable tunnel wizard, DNS, and teardown |
| [docs/gateway-self-hosting.md](docs/gateway-self-hosting.md) | Deploying and operating a gateway |
| [docs/updates.md](docs/updates.md) | Verified releases, phone-driven upgrades, Herdr compatibility |
| [docs/security.md](docs/security.md) | What is encrypted, what an intermediary sees, the audit log |
| [docs/development.md](docs/development.md) | Building, testing, and contributing |

## Security in one paragraph

Prompts, terminal output, uploads, and push details are encrypted end to end
between the phone and the relay. Whatever carries the traffic — a Cloudflare
tunnel or a gateway — observes connection metadata only, never plaintext; on the
direct path no application data reaches it at all, though a gateway still
answers address discovery. The relay exposes no write action to the workspace
inspector, and the app can require device verification before it reconnects.
[Details →](docs/security.md)

## License

[GNU Affero General Public License v3.0 or later](LICENSE).
