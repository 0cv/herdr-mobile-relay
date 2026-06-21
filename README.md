# herdi

Mobile interface for [herdr](https://herdr.dev) AI coding agents. Monitor agent status, approve requests, and send responses from your phone.

## Architecture

```
┌─────────────┐  ┌─────────────┐  ┌─────────────┐  ┌──────────┐
│  iOS App    │  │  Mac Menu   │  │  TUI        │  │ Telegram │
│  (SwiftUI)  │  │  Bar App    │  │  (Textual)  │  │ Bot      │
└──────┬──────┘  └──────┬──────┘  └──────┬──────┘  └────┬─────┘
       │                 │                │               │
       └────────────── WebSocket ─────────┴───────────────┘
                         │
                  ┌──────┴──────┐
                  │ herdi-relay │ :8375
                  └──────┬──────┘
                         │ CLI / UDP :8376
                  ┌──────┴──────┐
                  │    herdr    │
                  └─────────────┘
```

- **relay/** — Python daemon that polls herdr, accepts plugin events, and serves a WebSocket
- **herdi-ios/** — SwiftUI iOS app that connects via Bonjour or manual IP
- **herdi-mac/** — Native macOS menu bar app (like cmux)
- **relay/herdi_tui.py** — Terminal dashboard (Textual TUI)
- **relay/herdi_telegram.py** — Telegram bot for remote approval

## Install — macOS Menu Bar App

Download the latest DMG from [Releases](https://github.com/dcolinmorgan/herdi/releases), or build from source:

```bash
cd herdi-mac
./build.sh
# Output: dist/HerdiMac.app
cp -r dist/HerdiMac.app /Applications/
open /Applications/HerdiMac.app
```

The app lives in your menu bar. Toggle "Launch at Login" in the panel to start automatically.

## Install — Terminal TUI

```bash
pip install textual websockets
python3 relay/herdi_tui.py

# Or split into a herdr pane:
./relay/herdi-dash.sh
```

## Setup

### Relay (on your Mac)

```bash
cd relay
pip install -r requirements.txt
python3 herdi_relay.py

# Or install as herdr plugin for instant event push:
herdr plugin link .
```

### iOS App

Open `herdi-ios/` in Xcode or build with Swift Package Manager. Requires iOS 17+.

The app auto-discovers the relay via Bonjour (`_herdi._tcp`), or you can enter the IP manually in Settings.

## Features (MVP)

- Agent kanban board (Blocked → Working → Idle)
- Tap blocked agents to see approval prompt + option buttons
- Send responses back to the agent with one tap
- Auto-reconnect on network changes
- Bonjour service discovery

## LaunchAgent

To keep the relay running:

```bash
cp relay/com.herdi.relay.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.herdi.relay.plist
```

## Telegram Bot

Approve/reject agent requests from anywhere via Telegram:

```bash
# 1. Create a bot via @BotFather, get token
# 2. Send /start to your bot to get your chat ID
# 3. Set env vars:
export HERDI_TG_TOKEN="your-token"
export HERDI_TG_CHAT_ID="your-chat-id"

# 4. Run alongside the relay:
python3 relay/herdi_telegram.py
```

When an agent blocks, you get a Telegram message with inline buttons:
- **✅ Yes (once)** → `yes, single permission`
- **🔓 Trust (always)** → `trust, always allow`
- **❌ No** → `no (tab to edit)`

For subagent approvals:
- **✅ Approve all** → `approve all pending`
- **⚙️ Configure** → `configure individually`
- **❌ Cancel** → `exit (cancel subagents)`

You can also reply to any notification with free text to send a custom response.
