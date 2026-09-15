# Slash-command discovery measurements

These measurements cover the bounded catalog and the installed-PWA rendering
path. They are retained with the release identity so a future UI or transport
change can be compared against the same workload.

## Workload and release

- Release served from `web/`: version `0.21.3`, assets `382`, build
  `d94d3346c9244736e18853b5248911dd13649ac3b6d22e585b19a88ac211e6e5`.
- Browser fixture returned 4,096 commands with 240-character descriptions and
  120-character argument hints, then included `/late-command` at a late index.
- The phone filtered the retained catalog before rendering its 200-row display
  cap. A broad `/` query rendered 200 options and the display-limit guidance;
  `/late` rendered the late command and selection filled the composer without a
  `submit_prompt` message.

## Results

Chromium's Playwright `Pixel 7` mobile project, with the PWA standalone flags
set, passed three release-build runs:

```text
HERDR_WEB_ROOT=../web npm --prefix frontend run test:browser -- --project=chromium-mobile --grep 'keeps a large slash catalog responsive'
```

Observed `/late` filter-to-render timings were **28.9 ms, 32.3 ms, and
49.9 ms** (28.9–49.9 ms). The test also verified that the page's loaded
`/version.json` matched the release identity above.

The Go regression exercises the real Hub/WebSocket delivery path with worst-case
JSON escaping. It delivered **1,870 commands in 4,192,740 serialized bytes**
against the **4,194,304-byte** outbound message limit, with `truncated: true`.
The test command was:

```text
go test ./internal/app -run TestSlashCommandCatalogFitsOutboundHubBudget -count=1 -v
```

## Browser and device coverage

The direct host Playwright command cannot launch `webkit-mobile` (`iPhone 15`)
because the host lacks `libicu74` and `libjpeg-turbo8`. The project's official
`make frontend-browser-release` wrapper runs the same release suite in the
version-matched Playwright container; that run passed **104 Chromium-mobile and
104 WebKit-mobile tests**, including the large-catalog test (observed filter
samples: 42.3 ms on Chromium and 36.0 ms on WebKit).

No retained physical iOS or Android device was available for this run. The
Pixel 7 and iPhone 15 projects are browser/device emulation, not physical
hardware. Physical-device verification remains **unperformed**, not passed.
For this issue's review scope, the human explicitly accepted the documented
emulation-only evidence; that acceptance does not change the physical-device
status or authorize device access, deployment, or infrastructure changes.
