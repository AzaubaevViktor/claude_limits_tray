from datetime import datetime, timedelta, timezone

import pytest

import helpers as h


# --- formatting -------------------------------------------------------------

@pytest.mark.parametrize("secs,expected", [
    (None, "—"), (0, "<1m"), (30, "<1m"), (90, "1m"),
    (90 * 60, "1h30m"), (50 * 3600, "2d 2h"),
])
def test_fmt_remaining(secs, expected):
    assert h.fmt_remaining(secs) == expected


@pytest.mark.parametrize("secs,expected", [
    (5, "5s ago"), (75, "1m 15s ago"), (3700, "1h 1m ago"), (-3, "0s ago"),
])
def test_fmt_ago(secs, expected):
    assert h.fmt_ago(secs) == expected


# --- time helpers -----------------------------------------------------------

def test_parse_iso():
    assert h.parse_iso(None) is None
    assert h.parse_iso("nonsense") is None
    assert h.parse_iso("2026-06-25T21:00:00+00:00") is not None


def test_seconds_until_and_fraction():
    future = (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat()
    assert h.seconds_until(future) == pytest.approx(3600, abs=5)
    assert h.time_fraction(future, h.FIVE_HOUR_SECS) == pytest.approx(0.2, abs=0.01)
    assert h.time_fraction(None, h.WEEK_SECS) == 0.0


def test_wait_seconds():
    assert h.wait_seconds(None) is None
    assert h.wait_seconds("") is None
    epoch = str(int(datetime.now(timezone.utc).timestamp()) + 120)
    assert h.wait_seconds(epoch) == pytest.approx(120, abs=5)
    iso = (datetime.now(timezone.utc) + timedelta(seconds=90)).isoformat()
    assert h.wait_seconds(iso) == pytest.approx(90, abs=5)


# --- working-hours math -----------------------------------------------------

NOON = datetime(2026, 6, 25, 12, 0, 0)


def _reset(**delta):
    return (NOON + timedelta(**delta)).astimezone().isoformat()


def test_same_day_window():
    ws = h.working_seconds_until(_reset(hours=5), 9 * 60, 18 * 60, NOON)
    assert ws == pytest.approx(5 * 3600, abs=1)        # 12:00->17:00 all work


def test_clamped_to_work_end():
    now = datetime(2026, 6, 25, 17, 0, 0)
    reset = (now + timedelta(hours=5)).astimezone().isoformat()
    ws = h.working_seconds_until(reset, 9 * 60, 18 * 60, now)
    assert ws == pytest.approx(3600, abs=1)            # only 17:00->18:00


def test_weekly_nine_to_six():
    ws = h.working_seconds_until(_reset(days=7), 9 * 60, 18 * 60, NOON)
    assert ws / 3600 == pytest.approx(7 * 9, abs=0.1)  # 9h/day * 7


def test_overnight_window_17h():
    ws = h.working_seconds_until(_reset(hours=24), 9 * 60, 2 * 60, NOON)
    assert ws / 3600 == pytest.approx(17, abs=0.1)     # 09:00->02:00


def test_overnight_counts_from_previous_day():
    now = datetime(2026, 6, 25, 1, 0, 0)   # inside yesterday's 09:00->02:00
    reset = (now + timedelta(hours=2)).astimezone().isoformat()
    ws = h.working_seconds_until(reset, 9 * 60, 2 * 60, now)
    assert ws / 3600 == pytest.approx(1, abs=0.05)     # 01:00->02:00


def test_reset_in_past_is_zero():
    past = (NOON - timedelta(hours=1)).astimezone().isoformat()
    assert h.working_seconds_until(past, 9 * 60, 18 * 60, NOON) == 0.0


# --- backoff ----------------------------------------------------------------

def test_backoff_exponential_and_cap():
    assert h.backoff_seconds(1, 500, None, None, 0.0) == 15.0
    assert h.backoff_seconds(2, 500, None, None, 0.0) == 30.0
    assert h.backoff_seconds(3, 500, None, None, 0.0) == 60.0
    assert h.backoff_seconds(99, 500, None, None, 0.0) == 600.0


def test_backoff_429_floor_60():
    assert h.backoff_seconds(1, 429, 0, None, 0.0) == 60.0
    assert h.backoff_seconds(1, 429, None, None, 0.0) == 60.0


def test_backoff_429_uses_retry_after():
    assert h.backoff_seconds(1, 429, 300, None, 0.0) == 300.0


def test_backoff_429_reset_header_wins():
    reset = (datetime.now(timezone.utc) + timedelta(seconds=200)).isoformat()
    assert h.backoff_seconds(1, 429, 30, reset, 0.0) == pytest.approx(200, abs=5)


def test_backoff_jitter_only_adds():
    base = h.backoff_seconds(1, 429, 100, None, 0.0)
    hi = h.backoff_seconds(1, 429, 100, None, 0.25)
    assert base == 100.0
    assert hi == pytest.approx(125.0)
    assert hi >= base
