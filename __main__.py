"""Menu-bar monitor for Claude usage limits.

A circular template icon shows four gauges built from two concentric rings, plus
a centre Claude spark whose colour forecasts whether you'll run out of quota.
The dropdown spells everything out as numbers and shows the account.

Data comes from the same OAuth endpoint Claude Code's /usage screen uses
(api.anthropic.com/api/oauth/usage); the bearer token is read from the
"Claude Code-credentials" keychain item that Claude Code itself maintains.
"""

import atexit
import json
import math
import os
import random
import subprocess
import time
from collections import deque
from dataclasses import dataclass
from datetime import datetime, time as dtime, timedelta, timezone
from typing import Optional

import rumps
from AppKit import (NSFont, NSFontAttributeName, NSImage,
                    NSMutableParagraphStyle, NSParagraphStyleAttributeName,
                    NSTextAlignmentRight, NSTextTab)
from Foundation import NSAttributedString, NSData

FIVE_HOUR_SECS = 5 * 3600
WEEK_SECS = 7 * 24 * 3600

REFRESH_SECS = 30           # how often to poll the usage endpoint
PROFILE_EVERY = 60          # re-fetch the account profile every N ticks
FONT_SIZE = 11.0
CACHE_TTL = 300.0           # keep showing the last good response this long on errors
SAVE_EVERY_SECS = 300.0     # don't rewrite the poll-state file more often than this

# Remembers, across restarts, when it's safe to poll again — so relaunching
# doesn't immediately re-hit a 429.
_HERE = (os.path.dirname(os.path.abspath(__file__))
         if "__file__" in globals() else os.getcwd())
STATE_PATH = os.path.join(_HERE, ".poll-state.json")

PLAN_NAMES = {
    "default_claude_max_5x": "Claude Max 5×",
    "default_claude_max_20x": "Claude Max 20×",
    "claude_max": "Claude Max",
    "claude_pro": "Claude Pro",
}

# a limit is {"pct": used 0..100, "time_left": fraction of window remaining 0..1}
BLANK_LIMITS = {"session": {"pct": 100.0, "time_left": 1.0},
                "weekly": {"pct": 100.0, "time_left": 1.0}}


# --- pure helpers -----------------------------------------------------------

def parse_iso(value):
    if not value:
        return None
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        return None


def seconds_until(reset_at):
    dt = parse_iso(reset_at)
    if dt is None:
        return None
    return (dt - datetime.now(timezone.utc)).total_seconds()


def time_fraction(reset_at, window_secs):
    """Fraction of the window still remaining (0..1)."""
    remaining = seconds_until(reset_at)
    if remaining is None:
        return 0.0
    return max(0.0, min(1.0, remaining / window_secs))


def working_seconds_until(reset_at, ws_min, we_min, now=None):
    """Seconds of *working time* between now and the reset — counting only the
    daily [ws_min, we_min] window (minutes from local midnight). This is the
    time during which the quota can actually be consumed."""
    dt = parse_iso(reset_at)
    if dt is None:
        return None
    reset_local = dt.astimezone().replace(tzinfo=None)   # UTC -> naive local
    now_local = now or datetime.now()
    if reset_local <= now_local:
        return 0.0
    if we_min <= ws_min:        # misconfigured window; fall back to wall-clock
        return (reset_local - now_local).total_seconds()
    total = 0.0
    day = now_local.date()
    while day <= reset_local.date():
        ws = datetime.combine(day, dtime(ws_min // 60, ws_min % 60))
        we = datetime.combine(day, dtime(we_min // 60, we_min % 60))
        lo = max(ws, now_local)
        hi = min(we, reset_local)
        if hi > lo:
            total += (hi - lo).total_seconds()
        day += timedelta(days=1)
    return total


def wait_seconds(reset_at):
    """Seconds until a ratelimit-reset value (ISO timestamp or epoch seconds)."""
    if not reset_at:
        return None
    secs = seconds_until(reset_at)        # ISO timestamp
    if secs is not None:
        return secs
    try:
        return float(reset_at) - time.time()   # epoch seconds
    except (TypeError, ValueError):
        return None


def fmt_remaining(seconds):
    if seconds is None:
        return "—"
    seconds = max(0, int(seconds))
    if seconds < 60:
        return "<1m"
    minutes = seconds // 60
    if minutes < 60:
        return f"{minutes}m"
    hours, minutes = divmod(minutes, 60)
    if hours < 24:
        return f"{hours}h{minutes:02d}m"
    days, hours = divmod(hours, 24)
    return f"{days}d {hours}h"


_FRACTIONS = [
    (0.0, "0"), (1 / 8, "⅛"), (1 / 6, "⅙"), (1 / 5, "⅕"), (1 / 4, "¼"),
    (1 / 3, "⅓"), (3 / 8, "⅜"), (2 / 5, "⅖"), (1 / 2, "½"), (3 / 5, "⅗"),
    (5 / 8, "⅝"), (2 / 3, "⅔"), (3 / 4, "¾"), (4 / 5, "⅘"), (5 / 6, "⅚"),
    (7 / 8, "⅞"), (1.0, "1"),
]


def fmt_fraction(value):
    """Nearest vulgar fraction for a 0..1 value ('0', '¾', '1')."""
    value = max(0.0, min(1.0, value))
    return min(_FRACTIONS, key=lambda f: abs(f[0] - value))[1]


def fmt_ago(seconds):
    seconds = int(max(0, seconds))
    if seconds < 60:
        return f"{seconds}s ago"
    minutes, seconds = divmod(seconds, 60)
    if minutes < 60:
        return f"{minutes}m {seconds}s ago"
    hours, minutes = divmod(minutes, 60)
    return f"{hours}h {minutes}m ago"


def aligned(item, left, right, width=250.0):
    """Right-flush `right` using a tab stop (menus use a proportional font)."""
    ps = NSMutableParagraphStyle.alloc().init()
    tab = NSTextTab.alloc().initWithTextAlignment_location_options_(
        NSTextAlignmentRight, width, None)
    ps.setTabStops_([tab])
    attrs = {NSParagraphStyleAttributeName: ps,
             NSFontAttributeName: NSFont.menuFontOfSize_(0)}
    title = NSAttributedString.alloc().initWithString_attributes_(
        f"{left}\t{right}", attrs)
    item._menuitem.setAttributedTitle_(title)


# --- API client -------------------------------------------------------------

class ApiError(Exception):
    """HTTP/transport failure talking to the OAuth API."""

    def __init__(self, message, status=None, retry_after=None, reset_at=None):
        super().__init__(message)
        self.status = status
        self.retry_after = retry_after   # seconds, from the retry-after header
        self.reset_at = reset_at         # ISO/epoch, from a ratelimit-reset header


class UsageClient:
    """Reads the keychain token and calls the OAuth endpoints via system curl
    (curl uses the macOS trust store — more portable than Python's OpenSSL)."""

    BASE = "https://api.anthropic.com"
    KEYCHAIN = "Claude Code-credentials"
    USER_AGENT = "claude-cli/2.1.187 (external, cli)"
    OAUTH_BETA = "oauth-2025-04-20"
    META = "\n__META__"

    def _token(self):
        try:
            out = subprocess.check_output(
                ["security", "find-generic-password", "-s", self.KEYCHAIN, "-w"],
                text=True, stderr=subprocess.DEVNULL)
            oauth = json.loads(out)
            return oauth.get("claudeAiOauth", oauth)["accessToken"]
        except Exception as exc:
            raise ApiError("No credentials — sign in to Claude Code") from exc

    def _get(self, path):
        token = self._token()
        proc = subprocess.run(
            ["curl", "-sS", "--max-time", "15",
             "-H", f"Authorization: Bearer {token}",
             "-H", f"anthropic-beta: {self.OAUTH_BETA}",
             "-H", f"User-Agent: {self.USER_AGENT}",
             "-H", "Accept: application/json",
             "-w", f"{self.META}%{{http_code}}|%header{{retry-after}}"
                   "|%header{anthropic-ratelimit-unified-reset}",
             self.BASE + path],
            capture_output=True, text=True)
        if proc.returncode != 0:
            raise ApiError("Offline — can't reach Claude API")
        body, _, meta = proc.stdout.rpartition(self.META)
        fields = meta.split("|")
        try:
            status = int(fields[0])
        except (ValueError, IndexError):
            raise ApiError("Malformed curl response")
        if status >= 400:
            retry_after = None
            retry_s = fields[1].strip() if len(fields) > 1 else ""
            if retry_s:
                try:
                    retry_after = int(float(retry_s))
                except ValueError:
                    retry_after = None
            reset_at = (fields[2].strip() or None) if len(fields) > 2 else None
            raise ApiError(f"HTTP {status}", status=status,
                           retry_after=retry_after, reset_at=reset_at)
        return json.loads(body)

    def usage(self):
        return self._get("/api/oauth/usage")

    def profile(self):
        return self._get("/api/oauth/profile")


# --- burn-rate forecast -----------------------------------------------------

@dataclass
class Forecast:
    """Projects the recent consumption rate forward to a window's reset."""

    COLORS = {"purple": "rgb(175,82,222)", "green": "rgb(48,209,88)",
              "yellow": "rgb(255,204,0)", "red": "rgb(255,69,58)"}
    # gradient stops along the continuous severity axis (pos 0..1)
    STOPS = [(0.00, (175, 82, 222)),    # purple — lots of headroom
             (0.33, (48, 209, 88)),     # green  — finishing near zero
             (0.66, (255, 204, 0)),     # yellow — small overshoot
             (1.00, (255, 69, 58))]     # red    — large overshoot

    category: str
    spare: float
    break_secs: Optional[float]

    @classmethod
    def project(cls, pct, rate, ttr_secs):
        """rate is % of quota per second; ttr_secs is seconds to the reset.

          purple  > 10% of quota would still be spare at the reset
          green   0..10% spare (you'd use almost exactly your quota)
          yellow  you'd overshoot, but a break <= 1h makes it last
          red     overshoot needing a break > 1h
        """
        remaining = max(0.0, 100.0 - pct)
        if ttr_secs is None or rate <= 1e-9:
            return cls("purple", remaining, None)
        spare = remaining - rate * ttr_secs
        if spare > 10.0:
            return cls("purple", spare, None)
        if spare >= 0.0:
            return cls("green", spare, None)
        break_secs = ttr_secs - remaining / rate   # > 0 when overshooting
        return cls("yellow" if break_secs <= 3600.0 else "red", spare, break_secs)

    @property
    def pos(self):
        """Continuous severity, 0 (relaxed) .. 1 (way over budget)."""
        if self.break_secs is None:           # headroom: spare 30%+ .. 0%
            return 0.33 * (1.0 - max(0.0, min(1.0, self.spare / 30.0)))
        if self.break_secs <= 3600.0:         # overshoot up to a 1h break
            return 0.33 + 0.33 * (self.break_secs / 3600.0)
        over = (self.break_secs - 3600.0) / (3.0 * 3600.0)   # 1h..4h -> red
        return 0.66 + 0.34 * max(0.0, min(1.0, over))

    @classmethod
    def gradient(cls, pos):
        pos = max(0.0, min(1.0, pos))
        for (p0, c0), (p1, c1) in zip(cls.STOPS, cls.STOPS[1:]):
            if pos <= p1:
                t = (pos - p0) / (p1 - p0) if p1 > p0 else 0.0
                r, g, b = (round(c0[j] + (c1[j] - c0[j]) * t) for j in range(3))
                return f"rgb({r},{g},{b})"
        return "rgb(255,69,58)"

    @property
    def color(self):
        return self.gradient(self.pos)

    @property
    def text(self):
        if self.break_secs is not None:
            return f"over — pause {fmt_remaining(self.break_secs)} to last"
        tight = "" if self.category == "purple" else " (tight)"
        return f"{self.spare:.0f}% spare at reset{tight}"


class RateTracker:
    """Keeps a short history of usage samples and derives a consumption rate
    over the last WINDOW seconds (or since launch, while younger)."""

    WINDOW = 300.0
    MIN_DT = 10.0

    def __init__(self):
        self.history = deque(maxlen=64)   # (monotonic_t, sess_pct, week_pct)

    def update(self, now, sess_pct, week_pct):
        h = self.history
        if h and (sess_pct < h[-1][1] - 1.0 or week_pct < h[-1][2] - 1.0):
            h.clear()  # a window reset; restart the measurement
        h.append((now, sess_pct, week_pct))
        cutoff = now - self.WINDOW
        base = h[0]
        for sample in h:
            if sample[0] >= cutoff:
                base = sample
                break
        dt = now - base[0]
        if dt < self.MIN_DT:
            return 0.0, 0.0
        return (max(0.0, (sess_pct - base[1]) / dt),
                max(0.0, (week_pct - base[2]) / dt))


# --- the dial icon ----------------------------------------------------------

class Dial:
    """Renders the menu-bar dial from a declarative segment table. Edit
    SEGMENTS and both render() and legend_rows() follow."""

    CX = CY = 9.0
    SW = 1.7
    HALF_SHIFT = 0.9        # px each half moves off the vertical centreline
    END_GAP = 5.0           # degrees kept clear at the 12/6 o'clock ends
    RINGS = {"outer": 7.1, "inner": 4.3}
    SIDE_SIGN = {"left": -1, "right": +1}
    RING_LABEL = {"outer": "Outer ring", "inner": "Inner ring"}
    SIDE_LABEL = {"left": "left", "right": "right"}
    TRACK = "rgb(150,150,150)"
    GREEN, YELLOW, RED = (48, 209, 88), (255, 204, 0), (255, 69, 58)

    #   metric         arc length            colour (green plenty -> red low)
    #   percent_left   % of quota remaining  same  (long green arc = lots left)
    #   percent_used   % of quota used       headroom left (inverted vs arc)
    #   time_left      window time remaining same
    #   time_elapsed   time since last reset remaining (inverted: fills/reddens)
    SEGMENTS = [
        dict(ring="outer", side="right", source="session", metric="percent_left", bright=True,  label="Session left"),
        dict(ring="inner", side="right", source="session", metric="time_elapsed", bright=False, label="Session since reset"),
        dict(ring="outer", side="left",  source="weekly",  metric="percent_left", bright=True,  label="Weekly left"),
        dict(ring="inner", side="left",  source="weekly",  metric="time_elapsed", bright=False, label="Weekly since reset"),
    ]

    @classmethod
    def legend_rows(cls):
        rows = [(f"{cls.RING_LABEL[s['ring']]} · {cls.SIDE_LABEL[s['side']]}",
                 s["label"]) for s in cls.SEGMENTS]
        rows.append(("Colour", "green plenty → red low"))
        return rows

    @staticmethod
    def _metric(name, lim):
        pct_left = max(0.0, min(1.0, 1.0 - lim["pct"] / 100.0))
        t_left = max(0.0, min(1.0, lim["time_left"]))
        return {
            "percent_left": (pct_left, pct_left),
            "percent_used": (1.0 - pct_left, pct_left),
            "time_left": (t_left, t_left),
            "time_elapsed": (1.0 - t_left, t_left),
        }[name]

    @classmethod
    def _health_color(cls, health):
        health = max(0.0, min(1.0, health))
        if health >= 0.5:
            a, b, t = cls.YELLOW, cls.GREEN, (health - 0.5) / 0.5
        else:
            a, b, t = cls.RED, cls.YELLOW, health / 0.5
        r, g, bl = (round(a[i] + (b[i] - a[i]) * t) for i in range(3))
        return f"rgb({r},{g},{bl})"

    @classmethod
    def _arc(cls, cx, r, start_deg, end_deg):
        """Arc path between two angles measured clockwise from the top."""
        def point(deg):
            a = math.radians(deg)
            return cx + r * math.sin(a), cls.CY - r * math.cos(a)

        x0, y0 = point(start_deg)
        x1, y1 = point(end_deg)
        sweep = 1 if end_deg > start_deg else 0
        large = 1 if abs(end_deg - start_deg) > 180 else 0
        return (f"M{x0:.2f},{y0:.2f} "
                f"A{r:.2f},{r:.2f} 0 {large} {sweep} {x1:.2f},{y1:.2f}")

    @classmethod
    def _mark(cls, color):
        """A tiny Claude sparkle (radial burst) for the centre."""
        rays, r0, r1, out = 12, 0.35, 2.15, []
        for k in range(rays):
            ang = math.radians(k * 360.0 / rays)
            x0, y0 = cls.CX + r0 * math.cos(ang), cls.CY + r0 * math.sin(ang)
            x1, y1 = cls.CX + r1 * math.cos(ang), cls.CY + r1 * math.sin(ang)
            out.append(
                f'<line x1="{x0:.2f}" y1="{y0:.2f}" x2="{x1:.2f}" y2="{y1:.2f}" '
                f'stroke="{color}" stroke-width="0.82" stroke-linecap="round"/>')
        return "".join(out)

    @staticmethod
    def _image(svg):
        raw = svg.encode("utf-8")
        data = NSData.dataWithBytes_length_(raw, len(raw))
        img = NSImage.alloc().initWithData_(data)
        img.setSize_((18, 18))
        img.setTemplate_(False)   # coloured, so not a template image
        return img

    @classmethod
    def render(cls, limits, mark_color=None):
        """`limits` maps source -> limit dict. `mark_color=None` omits the
        centre mark (used for the error blink)."""
        cx = cls.CX
        span = 180.0 - 2 * cls.END_GAP
        parts = ['<svg xmlns="http://www.w3.org/2000/svg" width="18" '
                 'height="18" viewBox="0 0 18 18">']

        # faint tracks, one per ring/side, so the centre gap stays empty
        for sign in cls.SIDE_SIGN.values():
            ox = cx + sign * cls.HALF_SHIFT
            for r in cls.RINGS.values():
                d = cls._arc(ox, r, sign * cls.END_GAP, sign * (180.0 - cls.END_GAP))
                parts.append(
                    f'<path d="{d}" fill="none" stroke="{cls.TRACK}" '
                    f'stroke-width="{cls.SW}" stroke-opacity="0.45" '
                    f'stroke-linecap="round"/>')

        for seg in cls.SEGMENTS:
            length, health = cls._metric(seg["metric"], limits[seg["source"]])
            length = max(0.0, min(1.0, length))
            if length <= 0.004:
                continue
            sign = cls.SIDE_SIGN[seg["side"]]
            r = cls.RINGS[seg["ring"]]
            ox = cx + sign * cls.HALF_SHIFT
            start = sign * cls.END_GAP
            end = sign * (cls.END_GAP + length * span)
            alpha = 1.0 if seg["bright"] else 0.85
            parts.append(
                f'<path d="{cls._arc(ox, r, start, end)}" fill="none" '
                f'stroke="{cls._health_color(health)}" stroke-width="{cls.SW}" '
                f'stroke-linecap="round" stroke-opacity="{alpha}"/>')

        if mark_color is not None:
            parts.append(cls._mark(mark_color))
        parts.append("</svg>")
        return cls._image("".join(parts))


# --- the app ----------------------------------------------------------------

class ClaudeLimitsApp(rumps.App):

    def __init__(self):
        super().__init__("Claude Limits", title="…", template=True,
                         quit_button=None)
        self.client = UsageClient()
        self.rates = RateTracker()
        self.profile_data = None

        # runtime state
        self.icon_key = None
        self.tick = 0
        self.font_set = False
        self.error = False
        self.blink = False
        self.cached = False             # showing last good data despite an error
        self.last_ok = None
        self.last_fail = None
        self.last_fail_msg = None
        self.fail_count = 0
        self.retry_until = 0.0          # monotonic gate (this process)
        self.next_allowed_epoch = 0.0   # wall-clock gate (persisted)
        self._last_save = float("-inf")

        # working hours (local minutes from midnight); the forecast only counts
        # consumption during these hours. Default 09:00–18:00.
        self.work_start_min = 9 * 60
        self.work_end_min = 18 * 60
        self.last_usage = None          # last good response, for re-render
        self.sess_rate = 0.0
        self.week_rate = 0.0

        self._load_state()              # may defer the first poll after a restart
        self._build_menu()
        self.set_icon(Dial.render(BLANK_LIMITS), "init")
        self.refresh()
        self._timers = [
            rumps.Timer(self.refresh, REFRESH_SECS),
            rumps.Timer(self.tick_clock, 1),
            rumps.Timer(self.blink_tick, 0.5),
        ]
        for t in self._timers:
            t.start()
        atexit.register(lambda: self._save_state(force=True))

    # -- menu --------------------------------------------------------------

    def _build_menu(self):
        self.account_item = rumps.MenuItem("Account")
        self.plan_item = rumps.MenuItem("Plan")
        self.session_item = rumps.MenuItem("Session")
        self.session_reset_item = rumps.MenuItem("Session resets")
        self.weekly_item = rumps.MenuItem("Weekly")
        self.weekly_reset_item = rumps.MenuItem("Weekly resets")
        self.burn_parent = rumps.MenuItem("Forecast @ current rate")
        self.burn_session_item = rumps.MenuItem("Forecast session")
        self.burn_weekly_item = rumps.MenuItem("Forecast weekly")
        self.burn_parent.add(self.burn_session_item)
        self.burn_parent.add(self.burn_weekly_item)
        self.scoped_parent = rumps.MenuItem("Weekly by model")
        self.legend_parent = rumps.MenuItem("Legend (the dial)")
        for left, right in Dial.legend_rows():
            leg = rumps.MenuItem(left)
            aligned(leg, left, right, width=230.0)
            self.legend_parent.add(leg)
        self.status_item = rumps.MenuItem("Status")
        self.ok_item = rumps.MenuItem("Last success: —")
        self.fail_item = rumps.MenuItem("Last failure: —")
        self._build_work_menu()

        self.menu = [
            self.account_item, self.plan_item, None,
            self.session_item, self.session_reset_item,
            self.weekly_item, self.weekly_reset_item, None,
            self.burn_parent, self.scoped_parent, self.legend_parent, None,
            self.work_parent, None,
            self.status_item, self.ok_item, self.fail_item,
            rumps.MenuItem("Refresh now", callback=self.refresh_now),
            rumps.MenuItem("Quit", callback=self.on_quit),
        ]

    # -- working-hours settings --------------------------------------------

    @staticmethod
    def _fmt_hhmm(minutes):
        return f"{minutes // 60:02d}:{minutes % 60:02d}"

    @staticmethod
    def _parse_hhmm(text):
        h, m = text.split(":")
        return int(h) * 60 + int(m)

    def _build_work_menu(self):
        self.work_parent = rumps.MenuItem("Working hours")
        start_parent = rumps.MenuItem("Start")
        end_parent = rumps.MenuItem("End")
        self._start_items = {}
        self._end_items = {}
        for m in range(0, 24 * 60, 30):          # every half hour
            label = self._fmt_hhmm(m)
            si = rumps.MenuItem(label, callback=self.on_pick_start)
            ei = rumps.MenuItem(label, callback=self.on_pick_end)
            self._start_items[m] = si
            self._end_items[m] = ei
            start_parent.add(si)
            end_parent.add(ei)
        self.work_parent.add(start_parent)
        self.work_parent.add(end_parent)
        self._refresh_work_menu()

    def _refresh_work_menu(self):
        self.work_parent.title = (
            f"Working hours: {self._fmt_hhmm(self.work_start_min)}–"
            f"{self._fmt_hhmm(self.work_end_min)}")
        for m, it in self._start_items.items():
            it.state = 1 if m == self.work_start_min else 0
        for m, it in self._end_items.items():
            it.state = 1 if m == self.work_end_min else 0

    def _after_work_change(self):
        self._refresh_work_menu()
        self._save_state(force=True)
        if self.last_usage and not self.error:
            self._apply(self.last_usage)   # re-render forecast with new hours

    def on_pick_start(self, sender):
        self.work_start_min = self._parse_hhmm(sender.title)
        if self.work_end_min <= self.work_start_min:
            self.work_end_min = min(self.work_start_min + 30, 23 * 60 + 30)
        self._after_work_change()

    def on_pick_end(self, sender):
        self.work_end_min = self._parse_hhmm(sender.title)
        if self.work_start_min >= self.work_end_min:
            self.work_start_min = max(self.work_end_min - 30, 0)
        self._after_work_change()

    # -- status-bar icon ---------------------------------------------------

    def set_icon(self, image, key):
        if self.icon_key == key:
            return
        self.icon_key = key
        self._icon = "<generated>"          # marker so rumps keeps the image
        self._icon_nsimage = image
        try:
            self._nsapp.setStatusBarIcon()
        except AttributeError:
            pass  # before run(): rumps applies _icon_nsimage on launch

    def shrink_font(self):
        if self.font_set:
            return
        try:
            self._nsapp.nsstatusitem.button().setFont_(
                NSFont.systemFontOfSize_(FONT_SIZE))
            self.font_set = True
        except Exception:
            pass

    # -- persistence / lifecycle -------------------------------------------

    def _load_state(self):
        """Restore the persisted poll gate so a restart doesn't re-hit a 429."""
        try:
            with open(STATE_PATH) as f:
                data = json.load(f)
        except Exception:
            return
        ok = data.get("last_ok")
        if ok:
            try:
                self.last_ok = datetime.fromtimestamp(ok)
            except Exception:
                pass
        na = data.get("next_allowed")
        if na:
            delay = na - time.time()
            if delay > 0:
                self.retry_until = time.monotonic() + delay
                self.next_allowed_epoch = na
        for key, attr in (("work_start", "work_start_min"),
                          ("work_end", "work_end_min")):
            try:
                if data.get(key):
                    setattr(self, attr, self._parse_hhmm(data[key]))
            except Exception:
                pass

    def _save_state(self, force=False):
        """Write the next-allowed poll time, throttled (and forced on quit)."""
        now_m = time.monotonic()
        if not force and (now_m - self._last_save) < SAVE_EVERY_SECS:
            return
        self._last_save = now_m
        try:
            with open(STATE_PATH, "w") as f:
                json.dump({
                    "last_ok": self.last_ok.timestamp() if self.last_ok else None,
                    "next_allowed": self.next_allowed_epoch,
                    "work_start": self._fmt_hhmm(self.work_start_min),
                    "work_end": self._fmt_hhmm(self.work_end_min),
                }, f, indent=2)
        except Exception:
            pass

    def refresh_now(self, _=None):
        self.retry_until = 0.0   # explicit user action: bypass the backoff gate
        self.refresh()

    def on_quit(self, _=None):
        self._save_state(force=True)
        rumps.quit_application()

    # -- error handling ----------------------------------------------------

    def render_error_icon(self):
        """All-grey dial with a red Claude mark that blinks on/off."""
        on = self.blink
        self.set_icon(
            Dial.render(BLANK_LIMITS,
                        mark_color=Forecast.COLORS["red"] if on else None),
            f"err-{int(on)}")

    def blink_tick(self, _=None):
        if not self.error:
            return
        self.blink = not self.blink
        self.render_error_icon()

    def show_error(self, message):
        self.error = True
        self.render_error_icon()
        self.title = "?"
        self.status_item.title = f"⚠ {message}"
        for it in (self.account_item, self.plan_item, self.session_item,
                   self.session_reset_item, self.weekly_item,
                   self.weekly_reset_item):
            it.title = "—"

    @staticmethod
    def describe(exc):
        if exc.status == 429:
            return "Rate-limited (429)"
        if exc.status in (401, 403):
            return "Auth expired — run any Claude command"
        if exc.status:
            return f"Usage API error {exc.status}"
        return str(exc)   # transport / no-credentials message

    def note_fail(self, exc):
        """Record a failed request and arm a backoff.

        Wait precision, best first: a ratelimit-reset header, then retry-after,
        then an exponential floor. Add equal jitter, but never wait less than a
        server-mandated minimum. While the last good response is still fresh
        (< CACHE_TTL) keep showing it instead of the error dial."""
        self.last_fail = datetime.now()
        self.last_fail_msg = self.describe(exc)
        self.fail_count += 1
        base = min(600.0, 15.0 * (2 ** (self.fail_count - 1)))
        if exc.status == 429:
            server = wait_seconds(exc.reset_at) or exc.retry_after
            base = float(server) if (server and server > 0) else max(60.0, base)
        # jitter upward only — never poll sooner than the computed backoff
        backoff = base * (1.0 + random.uniform(0.0, 0.25))
        self.retry_until = time.monotonic() + backoff
        self.next_allowed_epoch = time.time() + backoff

        fresh = (self.last_ok is not None
                 and (datetime.now() - self.last_ok).total_seconds() < CACHE_TTL)
        if fresh:
            self.error = False     # keep the last good dial on screen
            self.cached = True
        else:
            self.cached = False
            self.show_error(f"{self.last_fail_msg} · retry {int(backoff)}s")
        self._save_state()
        self.tick_clock()

    # -- relative-time labels (1 s, no network) ----------------------------

    def tick_clock(self, _=None):
        def ago(ts):
            return fmt_ago((datetime.now() - ts).total_seconds()) if ts else "—"
        aligned(self.ok_item, "✓ Last success", ago(self.last_ok))
        aligned(self.fail_item, "✗ Last failure", ago(self.last_fail))
        left = self.retry_until - time.monotonic()
        if self.cached:
            age = ((datetime.now() - self.last_ok).total_seconds()
                   if self.last_ok else 1e9)
            if age >= CACHE_TTL:        # stale: fall back to the error dial
                self.cached = False
                self.show_error(f"No fresh data — {self.last_fail_msg or 'error'}")
            else:
                self.status_item.title = (
                    f"⚠ {self.last_fail_msg} — showing cached "
                    f"({int(CACHE_TTL - age)}s)")
        elif self.error and left > 0:
            self.status_item.title = f"⚠ backing off — retry in {int(left) + 1}s"

    # -- profile / scoped limits -------------------------------------------

    def update_profile(self):
        if self.profile_data is not None and self.tick % PROFILE_EVERY != 0:
            return
        try:
            self.profile_data = self.client.profile()
        except Exception:
            pass  # keep the last good profile
        prof = self.profile_data or {}
        acct = prof.get("account", {})
        org = prof.get("organization", {})
        name = acct.get("full_name") or acct.get("display_name") or "—"
        aligned(self.account_item, name, acct.get("email", ""))
        tier = org.get("rate_limit_tier", "")
        otype = org.get("organization_type", "")
        self.plan_item.title = "\U0001f397 " + (
            PLAN_NAMES.get(tier) or PLAN_NAMES.get(otype) or tier or "—")

    def render_scoped(self, limits):
        scoped = [l for l in limits
                  if l.get("kind") == "weekly_scoped" and l.get("scope")
                  and ((l.get("percent") or 0) > 0 or l.get("resets_at"))]
        try:
            self.scoped_parent._menu.removeAllItems()
        except Exception:
            pass
        self.scoped_parent._menuitem.setSubmenu_(None)
        if not scoped:
            self.scoped_parent.title = "Weekly by model: —"
            return
        self.scoped_parent.title = "Weekly by model"
        for l in scoped:
            model = (l.get("scope", {}).get("model", {}) or {}).get(
                "display_name", "model")
            child = rumps.MenuItem(model)
            reset = fmt_remaining(seconds_until(l.get("resets_at")))
            rem = 100.0 - float(l.get("percent", 0) or 0)
            aligned(child, model, f"{rem:.0f}% left  ·  {reset}")
            self.scoped_parent.add(child)

    # -- main poll ---------------------------------------------------------

    def refresh(self, _=None):
        self.shrink_font()
        self.tick += 1
        if time.monotonic() < self.retry_until:
            return  # still backing off from an earlier failure; don't poll
        try:
            data = self.client.usage()
        except ApiError as exc:
            self.note_fail(exc)
            return
        except Exception as exc:
            self.note_fail(ApiError(f"Error ({type(exc).__name__})"))
            return

        # success: clear error + backoff state
        self.error = False
        self.cached = False
        self.fail_count = 0
        self.retry_until = 0.0
        self.last_ok = datetime.now()
        self.next_allowed_epoch = time.time() + REFRESH_SECS

        five = data.get("five_hour") or {}
        week = data.get("seven_day") or {}
        self.sess_rate, self.week_rate = self.rates.update(
            time.monotonic(),
            float(five.get("utilization") or 0.0),
            float(week.get("utilization") or 0.0))
        self.last_usage = data
        self._apply(data)
        self.update_profile()
        self._save_state()

    def _apply(self, data):
        """Render the dial, labels and forecast from a usage response. Pure
        re-render (no network), so it can also run when settings change."""
        five = data.get("five_hour") or {}
        week = data.get("seven_day") or {}
        sess_pct = float(five.get("utilization") or 0.0)
        week_pct = float(week.get("utilization") or 0.0)
        sess_secs = seconds_until(five.get("resets_at"))
        week_secs = seconds_until(week.get("resets_at"))
        sess_tf = time_fraction(five.get("resets_at"), FIVE_HOUR_SECS)
        week_tf = time_fraction(week.get("resets_at"), WEEK_SECS)
        sess_rem = 100.0 - sess_pct
        week_rem = 100.0 - week_pct

        # forecast over *working* time until each reset, not raw wall-clock
        sess_work = working_seconds_until(five.get("resets_at"),
                                          self.work_start_min, self.work_end_min)
        week_work = working_seconds_until(week.get("resets_at"),
                                          self.work_start_min, self.work_end_min)
        sess_fc = Forecast.project(sess_pct, self.sess_rate, sess_work)
        week_fc = Forecast.project(week_pct, self.week_rate, week_work)
        worst = max((sess_fc, week_fc), key=lambda f: f.pos)

        limits = {"session": {"pct": sess_pct, "time_left": sess_tf},
                  "weekly": {"pct": week_pct, "time_left": week_tf}}
        self.set_icon(
            Dial.render(limits, mark_color=worst.color),
            f"{round(sess_pct)}-{round(week_pct)}-"
            f"{round(sess_tf*48)}-{round(week_tf*48)}-{round(worst.pos*60)}")
        # tray: remaining quota as fractions — weekly (left) · session (right),
        # matching the dial sides
        self.title = f"{fmt_fraction(week_rem / 100)} {fmt_fraction(sess_rem / 100)}"

        aligned(self.session_item, "\U0001f550 Session (5h)", f"{sess_rem:.0f}% left")
        aligned(self.session_reset_item, "      resets in", fmt_remaining(sess_secs))
        aligned(self.weekly_item, "\U0001f4c5 Weekly", f"{week_rem:.0f}% left")
        aligned(self.weekly_reset_item, "      resets in", fmt_remaining(week_secs))

        self.burn_parent.title = f"Forecast @ rate  ·  {worst.category}"
        aligned(self.burn_session_item, "Session (5h)",
                f"{self.sess_rate * 3600:.1f}%/h  ·  {sess_fc.text}")
        aligned(self.burn_weekly_item, "Weekly",
                f"{self.week_rate * 3600:.1f}%/h  ·  {week_fc.text}")

        self.render_scoped(data.get("limits") or [])
        self.status_item.title = (f"✓ sess {sess_rem:.0f}% left  ·  "
                                  f"week {week_rem:.0f}% left")
        self.tick_clock()


if __name__ == "__main__":
    ClaudeLimitsApp().run()
