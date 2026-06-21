#!/usr/bin/env python3
"""Herdi Telegram bot - approval notifications + inline response buttons."""
import asyncio, json, os, logging

from telegram import Update, InlineKeyboardButton, InlineKeyboardMarkup
from telegram.ext import Application, CallbackQueryHandler, CommandHandler, MessageHandler, ContextTypes, filters

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("herdi-tg")

TOKEN = os.environ["HERDI_TG_TOKEN"]
CHAT_ID = os.environ.get("HERDI_TG_CHAT_ID")  # restrict to your user
RELAY_WS = os.environ.get("HERDI_RELAY_WS", "ws://127.0.0.1:8375")

# Store pane_id per message for callback routing
pending: dict[int, str] = {}  # message_id -> pane_id


# --- Relay communication ---

async def send_to_relay(pane_id: str, text: str):
    """Send a response to the herdi relay via WebSocket."""
    import websockets
    msg = json.dumps({"type": "respond", "pane_id": pane_id, "text": text})
    async with websockets.connect(RELAY_WS) as ws:
        await ws.send(msg)


# --- Bot handlers ---

async def start(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    await update.message.reply_text(
        "herdi telegram bot active.\n"
        f"Chat ID: `{update.effective_chat.id}`\n"
        "Set HERDI_TG_CHAT_ID to this value.",
        parse_mode="Markdown"
    )


async def handle_callback(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle inline keyboard button presses."""
    query = update.callback_query
    await query.answer()

    data = json.loads(query.data)
    pane_id = data["pane_id"]
    response = data["response"]

    await send_to_relay(pane_id, response)
    await query.edit_message_reply_markup(reply_markup=None)
    await query.message.reply_text(f"✓ Sent: `{response}`", parse_mode="Markdown")


async def handle_text(update: Update, ctx: ContextTypes.DEFAULT_TYPE):
    """Handle free-text replies (reply to a blocked notification to send custom response)."""
    if not update.message.reply_to_message:
        return
    orig_id = update.message.reply_to_message.message_id
    pane_id = pending.get(orig_id)
    if not pane_id:
        await update.message.reply_text("Can't find which agent this is for. Reply to a blocked notification.")
        return
    await send_to_relay(pane_id, update.message.text)
    await update.message.reply_text(f"✓ Sent to agent")


# --- Notification sender (called from relay listener) ---

TOOL_BUTTONS = [
    ("✅ Yes (once)", "yes, single permission"),
    ("🔓 Trust (always)", "trust, always allow"),
    ("❌ No", "no (tab to edit)"),
]

SUBAGENT_BUTTONS = [
    ("✅ Approve all", "approve all pending"),
    ("⚙️ Configure", "configure individually"),
    ("❌ Cancel", "exit (cancel subagents)"),
]


def make_keyboard(pane_id: str, options: list[str] | None) -> InlineKeyboardMarkup:
    """Build inline keyboard from agent options."""
    if options and "trust" in " ".join(options).lower():
        buttons = TOOL_BUTTONS
    elif options and "approve all" in " ".join(options).lower():
        buttons = SUBAGENT_BUTTONS
    else:
        # Generic: use whatever options were provided
        buttons = [(opt, opt) for opt in (options or ["yes, single permission", "no (tab to edit)"])]

    keyboard = [
        [InlineKeyboardButton(label, callback_data=json.dumps({"pane_id": pane_id, "response": resp}))]
        for label, resp in buttons
    ]
    return InlineKeyboardMarkup(keyboard)


async def notify_blocked(app: Application, pane_id: str, agent: str, project: str, prompt: str, options: list[str] | None):
    """Send a blocked notification to Telegram with action buttons."""
    if not CHAT_ID:
        return
    text = f"🚨 *{agent}* blocked in `{project}`\n\n```\n{prompt[:500]}\n```"
    keyboard = make_keyboard(pane_id, options)
    msg = await app.bot.send_message(
        chat_id=int(CHAT_ID), text=text, parse_mode="Markdown", reply_markup=keyboard
    )
    pending[msg.message_id] = pane_id


# --- Relay listener (subscribes to blocked events) ---

async def relay_listener(app: Application):
    """Connect to relay WebSocket and listen for blocked events."""
    import websockets
    while True:
        try:
            async with websockets.connect(RELAY_WS) as ws:
                log.info(f"Connected to relay at {RELAY_WS}")
                async for raw in ws:
                    try:
                        msg = json.loads(raw)
                    except json.JSONDecodeError:
                        continue
                    if msg.get("type") == "blocked":
                        await notify_blocked(
                            app,
                            pane_id=msg["pane_id"],
                            agent=msg.get("agent", "unknown"),
                            project=msg.get("project", ""),
                            prompt=msg.get("prompt", ""),
                            options=msg.get("options"),
                        )
        except Exception as e:
            log.warning(f"Relay connection lost: {e}, reconnecting in 5s...")
            await asyncio.sleep(5)


# --- Main ---

def main():
    app = Application.builder().token(TOKEN).build()
    app.add_handler(CommandHandler("start", start))
    app.add_handler(CallbackQueryHandler(handle_callback))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, handle_text))

    # Run relay listener alongside the bot
    loop = asyncio.new_event_loop()
    asyncio.set_event_loop(loop)

    async def run():
        async with app:
            await app.start()
            await app.updater.start_polling()
            await relay_listener(app)

    loop.run_until_complete(run())


if __name__ == "__main__":
    main()
