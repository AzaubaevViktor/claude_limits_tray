# Claude Limits

<p align="center"><img src="dial.svg" width="160" alt="the dial icon"></p>

A tiny macOS menu-bar app that shows — at a glance — how much of your Claude
and Codex usage is left and whether your current pace will blow through it.

## What it solves

Claude Code / claude.ai usage runs against a **rolling 5-hour session limit** and
a **weekly cap**, and you usually only discover you've hit one when you get
rate-limited in the middle of something. This app keeps both limits in your menu
bar, projects your current **burn rate** forward — counting only your working
hours — and colours a warning *before* you run out, so you can pace yourself or
take a break instead of getting cut off.

## How to read the dial

Two circular dials side-by-side: Claude on the left, Codex on the right. Each
dial has two concentric rings, split left/right:

```
          outer ring          inner ring
  left     Short-window % left      Short window: time since reset
  right    Long-window  % left      Long window:  time since reset
```

- **Quota arcs** (outer) — how much quota is left, coloured by the same
  since-reset pace projection as the Claude Code statusline. The first 10% of a
  window normally stays grey because an early burst is too noisy to
  extrapolate. An obvious early burn (at least 30 minutes of history and 5% of
  real spend) breaks out of that learning state immediately:
  - ⚪ grey — reset time is unknown, or the window is still in that learning period
  - 🟣 purple — projected to leave more than 10% of the quota unused
  - 🟢 green — projected to use 90–100% by reset
  - 🟡 yellow — projected to run out, but only for a short tolerable gap before reset
  - 🔴 red — projected to run out with a longer gap before reset

  The tolerable gap is 30 minutes for a short (5h) window and 12 hours for a
  long (weekly) window. A light-grey tail on the session arc still means the
  weekly cap makes that part of the session quota unspendable.
- **Weekly notches** (outer right) — thin separators slice the weekly arc into
  **"one full 5-hour session" blocks**, so the notches still lit tell you *how
  many more whole sessions* of weekly budget you have. The block size is
  **measured from your own spending** (no synthetic default) — until a real spend
  lands the menu reads "calibrating…".
- **Time arcs** (inner) — grow **greener as the reset nears** (fresh quota is
  coming soon). Pick how they *fill* from **Time ring** in the menu — either by
  time *elapsed* (default) or by time *left*; the colour is the same either way.
  Fixed tick marks split each time ring into known units — the **session by
  hours**, the **weekly by days**.
- **Centre dot** — the worst known outer-ring pace category. An unknown/learning
  window does not mask a known warning from the other window; the dot is grey
  only while both windows are still unknown.
- **Single-limit mode** — when only **one** window is in play (e.g. Codex now
  hands out only a weekly limit) the dial auto-collapses to **one big full
  circle** for that window instead of a half-empty pair of semicircles.
- **Thin edge ring (credits)** — drawn only when the provider has a credit
  budget (Claude *extra usage* / usage credits, Codex purchased credits). The
  whole circle is the **time until the longest window resets**; the lit arc,
  from the top clockwise, is **how much of that time the balance covers at the
  recent (15‑minute) burn rate** — e.g. credits that last two days with four
  days to the reset light half the ring. It reddens as the cover shrinks
  (green = lasts to reset). The **lighter head** of the arc is **the next 24
  hours' spend** at that rate. A plain grey ring means the budget exists but
  can't be projected yet (no rate measured or no reset time); a red ring means
  the balance is exhausted. No budget → no ring, the icon is as before.
- On an error that side's dial goes all-grey with a **red blinking** dot —
  Claude and Codex each blink on their own.

Click it for everything as numbers, plus your account.

## What it can do

- Live **session (5h)** and **weekly** limits — % remaining and time to reset.
- A **burn-rate forecast** per window, projected over your **working hours**
  (so the weekly burn reflects ~9h workdays, not 24h of wall-clock).
- Configurable **working hours** (30-min steps, default 09:00–18:00, may wrap
  past midnight like 09:00–02:00) — picked from the dropdown, saved to disk.
- **Burn-driven icon size** — the menu-bar icon **grows while tokens are
  actively burning** (18→24 pt, scored per window so a fast weekly burn and a
  fast 5h burn count equally) and shrinks back when idle. On by default; turn it
  off to pin a fixed **icon size** (14–24 pt, default 19). Both this toggle and
  the **Time ring** fill direction are picked from the dropdown and saved to disk.
- **Weekly limit in 5-hour-session blocks** — the dropdown spells out the dial's
  notches as "≈ N / M × 5h blocks left", with the calibrated cost of one block
  (weekly-% per session) and the spend basis it was measured from, globally and
  **per model**.
- Consumption **speed** two ways — a **recent** rate and the **average since the
  window opened** — with session in **%/h** and weekly in **%/d**, each with the
  **spare left at reset** if that pace holds. The recent line is labelled with
  the **actual span it was measured over** (nominally ~15 min, but widening to the
  real gap between polls when they're sparse — e.g. "Last 12m" — so a slow or
  manual refresh never misreads as 0).
- **Paired burn history** at the top of the menu — Claude is always the left
  card and Codex the right card, with large high-resolution timelines and
  separate totals/scales for the last hour, short window, and long window. Each
  independent scale has three speed labels on the right in `%/h` or `%/d`, plus
  start/midpoint/end time labels along the bottom. Every bar is one real interval
  between successful API updates, with its width matching the polling gap; its
  height and colour show consumption speed relative to the dashed sustainable
  pace. The
  top-right total is explicitly labelled **SPENT**. Each card also has
  a purple → green → yellow → red speedometer with
  its speed label beside it: **%/hour** on sub-day charts and **%/day** on daily
  charts. The needle reads the rolling **last 15 minutes** against the pace you
  can still **afford before the reset** (quota left ÷ time left), not against a
  flat whole-window pace: with lots of quota and a reset minutes away the
  needle relaxes even under heavy use, while a thin remainder with a long wait
  pushes it into red at a modest rate. Three fixed notches mark **½×**, **1×**
  (runs out exactly at reset) and **1½×** of that affordable pace, and a small
  accent-coloured pointer outside the arc shows where the plain whole-window
  pace currently sits on that scale — left of 1× you can go faster than
  normal, right of it even normal is too fast. If the reset time is unknown the
  needle falls back to the whole-window pace. The quota-end ETA also uses the
  15-minute rate (including how long ago the last increase was), not the
  average since the window opened. Each card separately labels, in fixed distinct colours, the
  **time to reset**, the quota runway at the **current 15-minute rate**, and the
  runway at the **average rate since reset**; the average label also includes
  that rate (`%/h` for 5-hour windows, `%/d` for weekly windows), including on
  the Codex 5-hour card. A smaller concentric semicircular bar compares the last 5
  minutes with the 15-minute rate: centre means unchanged, right means
  accelerating (green → yellow → red), and left means slowing
  (green → blue → purple). If polling data goes stale, the rate expires and the
  card stops projecting an exhaustion time. A
  weekly-only Codex limit is placed in the long-window row even when the API
  calls it `primary`; its real utilisation history is also shown as a 5-hour
  slice in the middle row instead of leaving that Codex card empty.
- **Credits runway** for both providers — a `Credits:` row with the balance
  (and the monthly cap / % used where Claude reports one) plus an indented
  detail row: how long the balance lasts at the recent rate **against the time
  to reset** and what one more day costs (`lasts 2d 3h of 4d 2h to reset (52%)
  · ≈ $6.10/day`). Claude's figure comes from `spend` (usage credits) when
  enabled, otherwise from `extra_usage`; extra-usage amounts are read as minor
  units scaled by `decimal_places`. Codex credits are tracked as a running
  counter of balance drops, since its API only reports the balance.
- A fourth **Credits 5h** chart row (Claude left, Codex right) in credit units:
  bars are credits spent per poll interval, the dashed line is the pace that
  spends the whole balance exactly by the reset, `AVG` is the last 24 h.
- **Per-model** weekly limits.
- Two peer dropdown blocks — **Claude** and **Codex** — each with its plan,
  both usage windows, forecast, speed, and per-model/additional limits (Codex
  also shows the signed-in account, credits, and available rate-limit resets).
- Shows the active Claude account and provides **Use current Claude login** to
  reload `Claude Code-credentials` immediately after switching accounts.
- Polls in the background with a live "requesting…" indicator; on errors it
  keeps showing the **last good values for 10 minutes** before going grey.
  The bottom of the menu lists **separate last success / last failure** rows
  for Claude and for Codex, the failure row carrying the error it hit.
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
`Claude Code-credentials` keychain item, which Claude Code keeps fresh while you
work. When that token is still valid the app just uses it (never interfering);
when it's expired and Claude Code is idle (e.g. overnight) the app mints a fresh
one itself — the same request Claude Code 2.1.263 sends
(`platform.claude.com/v1/oauth/token`, its client id and scope list) — and
**writes the new token pair back into the keychain item** exactly as Claude
Code does (`security -i`, payload hex-encoded on stdin, every other field of
the item preserved). Refresh tokens rotate, so without the write-back a
self-refresh would strand Claude Code's own copy. The keychain's refresh token
is always tried first (Claude Code may have rotated it since the app last
looked); the app's persisted copy is only a fallback. A failed refresh is
reported as such in the status (`Claude token refresh rate-limited (429)`,
`Claude refresh token rejected …`), never retried more than once per two
minutes, and never followed by a request with the expired token. If the
refresh token itself is rejected it is dead until you reopen Claude Code, and
the status says so. After changing accounts in Claude Code,
click **Use current Claude login** in the Claude menu to discard the old cached
token and immediately reload the current keychain credentials, profile, and
limits.

For Codex it reads the ChatGPT-authenticated Codex usage endpoint
(`chatgpt.com/backend-api/wham/usage`). On first run the token is bootstrapped
the same way the Codex CLI stores it: `CODEX_ACCESS_TOKEN`, `~/.codex/auth.json`,
or the macOS keychain item `Codex Auth`. From then on it keeps the **refresh
token** in its state file and mints fresh access tokens itself (OpenAI's OAuth
endpoint, the same flow the Codex CLI uses) — so the keychain is touched at most
once and you won't be re-prompted for a password on every launch. It only falls
back to the keychain if refreshing fails (e.g. you signed out and back in), or
when you click **Use current Codex login** in the Codex menu to replace a stale
saved token after switching accounts. Keep Codex signed in with ChatGPT
(`codex login`); API-key auth does not expose ChatGPT plan limits. Requests go
through the system `curl`; no network or crypto libraries are added.

## Code

A single Go program (SVG icon rendering, menu bar via a repo-local patched
`getlantern/systray`), split into files:

- `helpers.go` — pure time/format/backoff functions (no GUI deps).
- `api.go` — `Client` (keychain token + curl) and `APIError`.
- `forecast.go` — `Forecast` (burn projection + gradient) and `RateTracker`.
- `credits.go` — credit budgets (Claude extra usage / spend, Codex balance),
  their runway projection, and the credits menu text.
- `history.go` — per-series utilisation log and the burn-history charts.
- `dial.go` — `renderDial`, the SVG icon drawn from the `SEGMENTS` table.
- `app.go` — `App`: polling loop, state, and rendering.
- `menu.go` — menu-bar layout and the working-hours pickers.
- `main.go` — thin entry point.

```sh
go build -o claude-limits . && go test ./...
```
