# Herdr Mobile Relay Quick Start

Get mobile approval for local herdr agents with a temporary Cloudflare quick tunnel.

## 1. Start the relay and tunnel

```bash
./relay/start.sh
```

The script prints:

```text
Tunnel URL: https://example.trycloudflare.com
WebSocket:  wss://example.trycloudflare.com
Token:      0123456789abcdef0123456789abcdef
```

Quick tunnel hostnames are temporary and change when the tunnel restarts. The token is stored in `relay/.env` and reused on later quick-start runs.

## 2. Open the web app

Open your deployed copy of `web/` on your phone. In Settings, add the printed `wss://...trycloudflare.com` URL and token.

To deploy the web app:

```bash
cp .env.example .env
# edit WEB_PROJECT in .env if needed
make web-deploy
```

## 3. Use stable relay hostnames

For fixed `wss://` URLs, create one named Cloudflare tunnel and one DNS hostname per computer:

```bash
cloudflared tunnel login
cloudflared tunnel create herdr-mobile-relay-mac
cloudflared tunnel route dns herdr-mobile-relay-mac relay-mac.yourdomain.com

cloudflared tunnel create herdr-mobile-relay-fedora
cloudflared tunnel route dns herdr-mobile-relay-fedora relay-fedora.yourdomain.com
```

Install the background service for the current computer:

```bash
# macOS
make macos-service-install

# Fedora/Linux
make linux-service-install
```

Then use each computer's `wss://` hostname in the web app.

## 4. Show two computers on one page

Run one relay and one Cloudflare tunnel per computer. Give each computer a distinct hostname:

```text
wss://relay-mac.yourdomain.com
wss://relay-fedora.yourdomain.com
```

In the web app Settings, add both relay URLs. The browser connects to both relays directly and merges the agents on one page.
