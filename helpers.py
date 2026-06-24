"""Pure time / formatting / backoff helpers (no GUI dependencies)."""

import time
from datetime import datetime, time as dtime, timedelta, timezone

FIVE_HOUR_SECS = 5 * 3600
WEEK_SECS = 7 * 24 * 3600


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
    daily working window (minutes from local midnight). The window may wrap past
    midnight (e.g. 09:00 -> 02:00); end <= start means it runs into the next day.
    This is the time during which the quota can actually be consumed."""
    dt = parse_iso(reset_at)
    if dt is None:
        return None
    reset_local = dt.astimezone().replace(tzinfo=None)   # UTC -> naive local
    now_local = now or datetime.now()
    if reset_local <= now_local:
        return 0.0
    duration = (we_min - ws_min) % (24 * 60) or 24 * 60   # 0 -> round the clock
    total = 0.0
    # start a day early so an overnight window opened "yesterday" is counted
    day = now_local.date() - timedelta(days=1)
    while day <= reset_local.date():
        start = datetime.combine(day, dtime(ws_min // 60, ws_min % 60))
        end = start + timedelta(minutes=duration)
        lo = max(start, now_local)
        hi = min(end, reset_local)
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


def backoff_seconds(fail_count, status, retry_after, reset_at, jitter=0.0):
    """Seconds to wait before the next poll after a failure. Precision, best
    first: a ratelimit-reset header, then retry-after, then an exponential
    floor. `jitter` (0..0.25) only adds on top — never below the base."""
    base = min(600.0, 15.0 * (2 ** (fail_count - 1)))
    if status == 429:
        server = wait_seconds(reset_at) or retry_after
        base = float(server) if (server and server > 0) else max(60.0, base)
    return base * (1.0 + jitter)


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


def fmt_ago(seconds):
    seconds = int(max(0, seconds))
    if seconds < 60:
        return f"{seconds}s ago"
    minutes, seconds = divmod(seconds, 60)
    if minutes < 60:
        return f"{minutes}m {seconds}s ago"
    hours, minutes = divmod(minutes, 60)
    return f"{hours}h {minutes}m ago"
