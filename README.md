# Claude Limits

<p align="center"><img src="dial.svg" width="160" alt="the dial icon"></p>

A tiny macOS menu-bar app that shows — at a glance — how much of your Claude
usage is left and whether your current pace will blow through it.

## What it solves

Claude Code / claude.ai usage runs against a **rolling 5-hour session limit** and
a **weekly cap**, and you usually only discover you've hit one when you get
rate-limited in the middle of something. This app keeps both limits in your menu
bar, projects your current **burn rate** forward — counting only your working
hours — and colours a warning *before* you run out, so you can pace yourself or
take a break instead of getting cut off.

## How to read the dial

One circular icon, two concentric rings, split left/right:

```
          outer ring          inner ring
  left     Session % left      Session: time since reset
  right    Weekly  % left      Weekly:  time since reset
```

- **Percent arcs** (outer) — how much quota is left: green plenty → red low.
- **Time arcs** (inner) — fill as the window elapses and grow **greener as the
  reset nears** (fresh quota is coming soon).
- **Centre spark** — the burn-rate forecast (the worse of the two windows):
  - 🟣 purple — >10% of quota will still be spare at the reset
  - 🟢 green — you'll finish right around 0% (on track)
  - 🟡 yellow — you'd overshoot, but a ≤1h break saves it
  - 🔴 red — overshoot needing a >1h break
- On an error the dial goes all-grey with a **red blinking** spark.

Click it for everything as numbers, plus your account.

## What it can do

- Live **session (5h)** and **weekly** limits — % remaining and time to reset.
- A **burn-rate forecast** per window, projected over your **working hours**
  (so the weekly burn reflects ~9h workdays, not 24h of wall-clock).
- Configurable **working hours** (30-min steps, default 09:00–18:00, may wrap
  past midnight like 09:00–02:00) — picked from the dropdown, saved to disk.
- Current consumption **speed** (%/h), and **per-model** weekly limits.
- Polls in the background with a live "requesting…" indicator; on errors it
  keeps showing the **last good values for 10 minutes** before going grey.
- **Adaptive polling**: starts at one poll every **2 minutes** and self-tunes —
  each `429` grows the interval (×1.5, up to 10 min) and a run of clean polls
  shrinks it back — so it converges on a rate the usage endpoint accepts instead
  of hammering it every 30 s. The interval is remembered across restarts.
- **Backoff that respects the server**: on `429` it honours `retry-after` /
  ratelimit-reset headers, otherwise exponential with jitter capped at **10
  minutes** — and remembers the next-allowed poll time across restarts so
  relaunching doesn't immediately re-hit a 429. Failures that only clear when
  something external recovers — a **network outage** or an **expired token**
  (401/403, waiting for Claude Code to refresh it) — don't back off: they retry
  **once a minute** until they recover.
- **Request/response logging**: each call to the usage API — its status,
  duration, and body — is logged to **stderr**; the bearer token is never
  logged.
- Caches the **account, plan, last quota values and rate**, and saves on quit
  **and Ctrl-C** — so a restart is populated instantly.

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

## Data source

It reads the same OAuth endpoint Claude Code's `/usage` screen uses
(`api.anthropic.com/api/oauth/usage`). The bearer token comes from the
`Claude Code-credentials` keychain item that Claude Code keeps refreshed — so
**keep Claude Code signed in**. Requests go through the system `curl`; no
network or crypto libraries are added.

## Code

A single Go program (drawn with `fogleman/gg`, menu bar via
`getlantern/systray`), split into files:

- `helpers.go` — pure time/format/backoff functions (no GUI deps).
- `api.go` — `Client` (keychain token + curl) and `APIError`.
- `forecast.go` — `Forecast` (burn projection + gradient) and `RateTracker`.
- `dial.go` — `renderDial`, the icon drawn from the `SEGMENTS` table.
- `app.go` — `App`: polling loop, state, and rendering.
- `menu.go` — menu-bar layout and the working-hours pickers.
- `main.go` — thin entry point.

```sh
go build -o claude-limits . && go test ./...
```
