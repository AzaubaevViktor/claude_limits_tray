"""The menu-bar app: rumps wiring, polling, state, working hours."""

import atexit
import json
import os
import queue
import random
import signal
import threading
import time
from datetime import datetime

import rumps
from AppKit import (NSFont, NSFontAttributeName, NSMutableParagraphStyle,
                    NSParagraphStyleAttributeName, NSTextAlignmentRight,
                    NSTextTab)
from Foundation import NSAttributedString

from api import ApiError, UsageClient
from dial import BLANK_LIMITS, Dial
from forecast import Forecast, RateTracker
from helpers import (FIVE_HOUR_SECS, WEEK_SECS, backoff_seconds, fmt_ago,
                     fmt_remaining, seconds_until, time_fraction,
                     working_seconds_until)

REFRESH_SECS = 30           # how often to poll the usage endpoint
PROFILE_EVERY = 60          # re-fetch the account profile every N polls
FONT_SIZE = 11.0
CACHE_TTL = 600.0           # keep showing the last good response this long on errors
SAVE_EVERY_SECS = 300.0     # don't rewrite the poll-state file more often than this

# Remembers, across restarts, when it's safe to poll again — so relaunching
# doesn't immediately re-hit a 429.
STATE_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                          ".poll-state.json")

PLAN_NAMES = {
    "default_claude_max_5x": "Claude Max 5×",
    "default_claude_max_20x": "Claude Max 20×",
    "claude_max": "Claude Max",
    "claude_pro": "Claude Pro",
}


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


class ClaudeLimitsApp(rumps.App):

    def __init__(self):
        super().__init__("Claude Limits", title="…", template=True,
                         quit_button=None)
        self.client = UsageClient()
        self.rates = RateTracker()
        self.profile_data = None

        # runtime state
        self.icon_key = None
        self.poll_count = 0
        self.font_set = False
        self.error = False
        self.blink = False
        self.cached = False             # showing last good data despite an error
        self.last_ok = None
        self.last_fail = None
        self.last_fail_msg = None
        self.fail_count = 0
        self.next_poll = 0.0            # monotonic time of the next allowed poll
        self.next_allowed_epoch = 0.0   # wall-clock gate (persisted)
        self._last_save = float("-inf")
        self._inflight = False          # a request is running on a worker thread
        self._req_start = 0.0
        self._results = queue.Queue()
        self._signals_installed = False

        # working hours (local minutes from midnight); the forecast only counts
        # consumption during these hours. Default 09:00–18:00.
        self.work_start_min = 9 * 60
        self.work_end_min = 18 * 60
        self.last_usage = None          # last good response, for re-render
        self.sess_rate = 0.0
        self.week_rate = 0.0

        self._load_state()              # may defer the first poll after a restart
        self._build_menu()
        if self.last_usage:
            self._apply(self.last_usage)   # restore last known dial + forecast
        else:
            self.set_icon(Dial.render(BLANK_LIMITS), "init")
        if self.profile_data:
            self._render_profile()         # restore account/plan immediately
        self._timers = [
            rumps.Timer(self.tick, 1),
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
        self.speed_item = rumps.MenuItem("Speed")
        self.burn_parent.add(self.burn_session_item)
        self.burn_parent.add(self.burn_weekly_item)
        self.burn_parent.add(rumps.separator)
        self.burn_parent.add(self.speed_item)
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
            self.burn_parent,
            self.scoped_parent, self.legend_parent, None,
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
        # end <= start is allowed: the window wraps past midnight (09:00–02:00)
        self.work_start_min = self._parse_hhmm(sender.title)
        self._after_work_change()

    def on_pick_end(self, sender):
        self.work_end_min = self._parse_hhmm(sender.title)
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
                self.next_poll = time.monotonic() + delay
                self.next_allowed_epoch = na
        for key, attr in (("work_start", "work_start_min"),
                          ("work_end", "work_end_min")):
            try:
                if data.get(key):
                    setattr(self, attr, self._parse_hhmm(data[key]))
            except Exception:
                pass
        try:
            self.sess_rate = float(data.get("sess_rate") or 0.0)
            self.week_rate = float(data.get("week_rate") or 0.0)
            self.last_usage = data.get("usage") or None
            self.profile_data = data.get("profile") or None
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
                    "sess_rate": self.sess_rate,   # last measured %/s, to recompute
                    "week_rate": self.week_rate,
                    "usage": self.last_usage,      # last good response, to re-render
                    "profile": self.profile_data,  # account/plan, to show on restart
                }, f, indent=2)
        except Exception:
            pass

    def refresh_now(self, _=None):
        self.next_poll = 0.0     # explicit user action: bypass the backoff gate
        if not self._inflight:
            self._start_request()

    def on_quit(self, _=None):
        self._save_state(force=True)
        rumps.quit_application()

    def _install_signals(self):
        """Install our signal handlers from inside the run loop, so rumps'
        own SIGINT handler (set at run() time) doesn't clobber them."""
        self._signals_installed = True
        for sig in (signal.SIGINT, signal.SIGTERM):
            try:
                signal.signal(sig, self._on_signal)
            except (ValueError, OSError):
                pass

    def _on_signal(self, signum, frame):
        """Save on Ctrl-C / SIGTERM (atexit doesn't fire on signals)."""
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
        """Record a failed request and arm a backoff. While the last good
        response is still fresh (< CACHE_TTL) keep showing it."""
        self.last_fail = datetime.now()
        self.last_fail_msg = self.describe(exc)
        self.fail_count += 1
        backoff = backoff_seconds(self.fail_count, exc.status, exc.retry_after,
                                  exc.reset_at, random.uniform(0.0, 0.25))
        self.next_poll = time.monotonic() + backoff
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

    # -- 1 s heartbeat: drain results, maybe poll, refresh status ----------

    def tick(self, _=None):
        if not self._signals_installed:
            self._install_signals()
        self.shrink_font()
        self._drain_results()
        if not self._inflight and time.monotonic() >= self.next_poll:
            self._start_request()
        self._update_status()

    def _update_status(self):
        def ago(ts):
            return fmt_ago((datetime.now() - ts).total_seconds()) if ts else "—"
        aligned(self.ok_item, "✓ Last success", ago(self.last_ok))
        aligned(self.fail_item, "✗ Last failure", ago(self.last_fail))
        if self._inflight:
            elapsed = time.monotonic() - self._req_start
            self.status_item.title = f"⟳ requesting… {elapsed:.0f}s"
            return
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
        elif self.error:
            left = self.next_poll - time.monotonic()
            if left > 0:
                self.status_item.title = f"⚠ backing off — retry in {int(left) + 1}s"

    # -- profile / scoped limits -------------------------------------------

    def _render_profile(self):
        """Render account/plan from the (already fetched) profile data."""
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

    def _start_request(self, _=None):
        """Kick off the usage (and, when due, profile) request on a worker
        thread so the run loop — timers, the 'requesting…' counter — stays live."""
        self._inflight = True
        self._req_start = time.monotonic()
        self.poll_count += 1
        want_profile = (self.profile_data is None
                        or self.poll_count % PROFILE_EVERY == 0)
        client, results = self.client, self._results

        def worker():
            try:
                data = client.usage()
            except ApiError as exc:
                results.put(("err", exc))
                return
            except Exception as exc:
                results.put(("err", ApiError(f"Error ({type(exc).__name__})")))
                return
            prof = None
            if want_profile:
                try:
                    prof = client.profile()
                except Exception:
                    prof = None
            results.put(("ok", (data, prof)))

        threading.Thread(target=worker, daemon=True).start()

    def _drain_results(self):
        """Apply a finished request's result on the main thread."""
        try:
            kind, payload = self._results.get_nowait()
        except queue.Empty:
            return
        self._inflight = False
        if kind == "ok":
            data, prof = payload
            self._on_success(data, prof)
        else:
            self.note_fail(payload)

    def _on_success(self, data, prof):
        self.error = False
        self.cached = False
        self.fail_count = 0
        self.last_ok = datetime.now()
        self.next_poll = time.monotonic() + REFRESH_SECS
        self.next_allowed_epoch = time.time() + REFRESH_SECS

        five = data.get("five_hour") or {}
        week = data.get("seven_day") or {}
        rates = self.rates.update(
            time.monotonic(),
            float(five.get("utilization") or 0.0),
            float(week.get("utilization") or 0.0))
        if rates is not None:        # keep the last good rate until a fresh one
            self.sess_rate, self.week_rate = rates
        self.last_usage = data
        if prof is not None:
            self.profile_data = prof
        self._apply(data)
        self._render_profile()
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
        self.title = ""   # icon only; the numbers live in the dropdown

        aligned(self.session_item, "\U0001f550 Session (5h)", f"{sess_rem:.0f}% left")
        aligned(self.session_reset_item, "      resets in", fmt_remaining(sess_secs))
        aligned(self.weekly_item, "\U0001f4c5 Weekly", f"{week_rem:.0f}% left")
        aligned(self.weekly_reset_item, "      resets in", fmt_remaining(week_secs))

        aligned(self.speed_item, "⚡ Speed (5m)",
                f"sess {self.sess_rate * 3600:.1f}%/h  ·  "
                f"week {self.week_rate * 3600:.1f}%/h")
        self.burn_parent.title = f"Forecast @ rate  ·  {worst.category}"
        aligned(self.burn_session_item, "Session (5h)", sess_fc.text)
        aligned(self.burn_weekly_item, "Weekly", week_fc.text)

        self.render_scoped(data.get("limits") or [])
        self.status_item.title = (f"✓ sess {sess_rem:.0f}% left  ·  "
                                  f"week {week_rem:.0f}% left")
        self._update_status()
