# Slash-command suggestions

The phone's slash-command palette is a bounded, best-effort catalog. Most
providers share a **2,000 custom command or skill-file budget** for each
provider discovery pass and return at most **4,096 entries**, including
builtins. The phone validates and retains the same 4,096-entry maximum. These
limits are shared in
[`contracts/fixtures/slash_command_limits.json`](../contracts/fixtures/slash_command_limits.json)
and are checked against the Go and TypeScript implementations.

Hermes intentionally has two independent 2,000-file passes: one for its
native project/profile skills and one for configured compatibility skill
folders. It can therefore inspect more than 2,000 custom files in one request,
while its final catalog and wire-size limits still apply. This preserves Hermes'
provider precedence and configured-folder behavior; it is not a promise of an
unbounded catalog.

A catalog can be incomplete when a discovery pass, the final entry cap, or
the serialized response-size guard is reached. One outbound relay message is
limited to 4 MiB; the relay clips a large serialized `command_result` before
sending it and sets `truncated: true`. The `truncated` flag remains
conservative: it is not an exact count of omitted eligible commands. The phone
also marks a catalog incomplete if an older or newer relay sends more than its
local cap.

Typing filters only the suggestions already loaded on the phone. Filtering
runs over the retained catalog before the UI applies a separate 200-row display
cap. If more matches remain, the palette says that more matching commands are
hidden and asks the user to keep typing; keyboard selection and ARIA indices
cover only the rendered rows. If a command is absent, send it manually; the
palette does not search the computer again. Suggestions are cached for the pane
and agent/worktree identity and are invalidated when the relay reconnects.
Reopening a terminal is not a filesystem watch or an explicit refresh.

The relay and phone bundle should be updated together so the larger catalog and
its guidance are available on the device. Release the bundled phone assets with
`make web-release`; do not edit `web/` by hand.
