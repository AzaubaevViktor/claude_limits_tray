import pytest

from forecast import Forecast, RateTracker


# --- Forecast.project categories -------------------------------------------

def test_idle_is_purple():
    fc = Forecast.project(20, 0.0, 2 * 3600)
    assert fc.category == "purple"
    assert fc.break_secs is None


def test_plenty_spare_purple():
    assert Forecast.project(20, 10 / 3600.0, 2 * 3600).category == "purple"


def test_exact_finish_green():
    fc = Forecast.project(50, 25 / 3600.0, 2 * 3600)   # ~0% spare
    assert fc.category == "green"
    assert fc.spare >= 0.0


def test_small_overshoot_yellow():
    fc = Forecast.project(60, 30 / 3600.0, 2 * 3600)
    assert fc.category == "yellow"
    assert 0 < fc.break_secs <= 3600


def test_big_overshoot_red():
    fc = Forecast.project(80, 40 / 3600.0, 4 * 3600)
    assert fc.category == "red"
    assert fc.break_secs > 3600


def test_no_reset_time_is_purple():
    assert Forecast.project(90, 1.0, None).category == "purple"


# --- gradient / pos ---------------------------------------------------------

def test_pos_is_monotonic_by_severity():
    purple = Forecast.project(10, 0, 3600).pos
    green = Forecast.project(50, 25 / 3600.0, 2 * 3600).pos
    yellow = Forecast.project(60, 30 / 3600.0, 2 * 3600).pos
    red = Forecast.project(80, 40 / 3600.0, 4 * 3600).pos
    assert purple < green < yellow < red


def test_gradient_endpoints():
    assert Forecast.gradient(0.0) == "rgb(175,82,222)"   # purple
    assert Forecast.gradient(1.0) == "rgb(255,69,58)"    # red
    assert Forecast.gradient(0.5).startswith("rgb(")


def test_color_matches_gradient_of_pos():
    fc = Forecast.project(20, 0, 3600)
    assert fc.color == Forecast.gradient(fc.pos)


def test_text():
    assert "spare" in Forecast.project(20, 0, 3600).text
    assert "pause" in Forecast.project(80, 40 / 3600.0, 4 * 3600).text


# --- RateTracker ------------------------------------------------------------

def test_none_until_min_dt():
    rt = RateTracker()
    assert rt.update(1000.0, 10, 2) is None       # first sample
    assert rt.update(1005.0, 11, 2) is None       # dt < MIN_DT


def test_rate_after_min_dt():
    rt = RateTracker()
    rt.update(1000.0, 10, 2)
    sess, week = rt.update(1020.0, 30, 2)         # +20% over 20s
    assert sess == pytest.approx(1.0, abs=1e-3)
    assert week == pytest.approx(0.0, abs=1e-3)


def test_reset_clears_history():
    rt = RateTracker()
    rt.update(1000.0, 80, 50)
    assert rt.update(1020.0, 5, 50) is None       # a drop restarts measurement
    assert len(rt.history) == 1


def test_never_negative():
    rt = RateTracker()
    rt.update(1000.0, 10, 2)
    sess, _ = rt.update(1020.0, 10, 2)            # flat
    assert sess >= 0.0
