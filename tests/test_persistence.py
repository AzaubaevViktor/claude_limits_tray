import os
from datetime import datetime, timezone

import pytest

import ui
from ui import ClaudeLimitsApp


def _bare():
    """A ClaudeLimitsApp without rumps machinery — just the state attributes
    the persistence methods touch."""
    a = ClaudeLimitsApp.__new__(ClaudeLimitsApp)
    a.last_ok = None
    a.next_allowed_epoch = 0.0
    a.next_poll = 0.0
    a._last_save = float("-inf")
    a.work_start_min = 9 * 60
    a.work_end_min = 18 * 60
    a.sess_rate = 0.0
    a.week_rate = 0.0
    a.last_usage = None
    return a


def test_round_trip(tmp_path, monkeypatch):
    monkeypatch.setattr(ui, "STATE_PATH", str(tmp_path / "state.json"))
    a = _bare()
    a.last_ok = datetime.now()
    a.next_allowed_epoch = datetime.now(timezone.utc).timestamp() + 45
    a.work_start_min, a.work_end_min = 9 * 60, 2 * 60     # overnight window
    a.sess_rate, a.week_rate = 0.0033, 0.0001
    a.last_usage = {"five_hour": {"utilization": 21}}
    a._save_state(force=True)

    b = _bare()
    b._load_state()
    assert b.work_end_min == 2 * 60          # overnight preserved
    assert b.sess_rate == pytest.approx(0.0033)
    assert b.week_rate == pytest.approx(0.0001)
    assert b.last_usage is not None          # last quota values cached
    assert b.last_ok is not None
    assert b.next_poll > 0.0                 # future gate deferred on restart


def test_save_is_throttled(tmp_path, monkeypatch):
    path = str(tmp_path / "state.json")
    monkeypatch.setattr(ui, "STATE_PATH", path)
    a = _bare()
    a._last_save = ui.time.monotonic()       # pretend we just saved
    a._save_state(force=False)               # throttled -> skipped
    assert not os.path.exists(path)
    a._save_state(force=True)                # forced (quit/Ctrl-C) -> writes
    assert os.path.exists(path)


def test_load_missing_file_is_safe(monkeypatch):
    monkeypatch.setattr(ui, "STATE_PATH", "/nonexistent/x.json")
    a = _bare()
    a._load_state()                          # must not raise
    assert a.work_start_min == 9 * 60        # defaults intact
