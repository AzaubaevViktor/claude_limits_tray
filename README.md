# Claude Limits

A macOS menu-bar app (rumps) that shows your Claude usage limits at a glance.

## The icon

A circular template icon, two concentric rings, each split left/right:

```
        usage %         time left
        (left)          (right)
outer   session (5h)    until 5h reset
inner   weekly          until weekly reset
```

The exact layout lives in one table — `SEGMENTS` in `__main__.py`. Edit a row
and both the dial **and** the dropdown legend follow. Current layout:

```
        outer ring        inner ring
right   Session % left    Session: time since reset
left    Weekly % left     Weekly:  time since reset
```

- **Percent arcs** show how much quota is *left*; green when plenty, red when low.
- **Time arcs** fill as the window elapses and redden toward the reset.
- A small gap separates the left/right halves; the percent arcs are brighter.

### Centre mark — burn-rate forecast

The little Claude spark in the centre is coloured by projecting your recent
consumption rate (last 5 min, or since launch) forward to each window's reset,
taking the **worse** of the 5-hour and weekly windows. The colour is a smooth
gradient along the severity axis:

- **purple** — >10% of quota would still be spare at the reset
- **green** — ~0% spare (you'll use almost exactly your quota)
- **yellow** — you'd overshoot, but a break of ≤1h makes it last
- **red** — overshoot needing a break >1h

The menu-bar text shows remaining quota as nearest vulgar fractions —
weekly (left) · session (right), matching the dial sides (e.g. `1 ⅘`).

## The dropdown

Everything as numbers, plus the account:

- Account name + email, and plan (e.g. Claude Max 5×)
- Session (5h): % left · resets in …
- Weekly: % left · resets in …
- Forecast @ current rate: the burn projection (with %/h rate) for the 5h
  and weekly windows
- Working hours: pick start/end in 30-min steps (default 09:00–18:00). The
  forecast only counts consumption during these hours, so the weekly burn
  reflects ~9h workdays rather than 24h wall-clock.
- Weekly by model (scoped limits, when present)
- Legend explaining each part of the dial (auto-generated from `SEGMENTS`)
- Time since the last successful and last failed request
- Refresh now · Quit

On errors the dial goes all-grey with a red blinking centre mark, and requests
back off exponentially. On a 429 the app stops polling until the cooldown passes
and honours the server's `retry-after` header when it carries a positive value
(otherwise a 60s floor that grows on repeated failures).

## Code shape

- `UsageClient` — keychain token + curl calls to the OAuth endpoints; raises
  `ApiError(status, retry_after)`.
Split into modules:

- `helpers.py` — pure time/format/backoff functions (no GUI deps).
- `api.py` — `UsageClient` (keychain token + curl) and `ApiError`.
- `forecast.py` — `Forecast` (burn projection + gradient) and `RateTracker`.
- `dial.py` — `Dial`, the SVG/NSImage rendered from the `SEGMENTS` table.
- `ui.py` — `ClaudeLimitsApp(rumps.App)`: menus, timers, polling, persistence.
- `__main__.py` — thin entry point.

## Data source

It reads the same OAuth endpoint Claude Code's `/usage` screen uses
(`api.anthropic.com/api/oauth/usage`). The bearer token comes from the
`Claude Code-credentials` keychain item that Claude Code keeps refreshed —
so keep Claude Code signed in. Requests go through the system `curl`.

## Run

```sh
./claude_limits.sh
```

### Start at login (optional)

Add `claude_limits.sh` as a Login Item, or via `launchd`:

```sh
cat > ~/Library/LaunchAgents/com.local.claude-limits.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.local.claude-limits</string>
  <key>ProgramArguments</key>
  <array><string>$PWD/claude_limits.sh</string></array>
  <key>RunAtLoad</key><true/>
</dict></plist>
PLIST
launchctl load ~/Library/LaunchAgents/com.local.claude-limits.plist
```

## Dev

Python 3.10 venv with `rumps` (pulls pyobjc). Created with:

```sh
uv venv --python 3.10 .venv
uv pip install --python .venv/bin/python rumps
```
