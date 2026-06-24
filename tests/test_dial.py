import pytest

from dial import BLANK_LIMITS, Dial


def test_metric_mapping():
    lim = {"pct": 21, "time_left": 0.55}
    assert Dial._metric("percent_left", lim) == pytest.approx((0.79, 0.79))
    assert Dial._metric("time_left", lim) == pytest.approx((0.55, 0.55))
    assert Dial._metric("percent_used", lim)[0] == pytest.approx(0.21)
    # time_elapsed: length = elapsed, and colour greens with elapsed (health =
    # elapsed too) -> the less time left, the greener
    assert Dial._metric("time_elapsed", lim) == pytest.approx((0.45, 0.45))


def test_time_arc_greener_as_less_time_left():
    # less time remaining -> higher health -> greener
    nearly_done = Dial._metric("time_elapsed", {"pct": 0, "time_left": 0.1})[1]
    just_reset = Dial._metric("time_elapsed", {"pct": 0, "time_left": 0.9})[1]
    assert nearly_done > just_reset
    assert Dial._health_color(nearly_done) != Dial._health_color(just_reset)


def test_legend_matches_segments():
    rows = Dial.legend_rows()
    assert len(rows) == len(Dial.SEGMENTS) + 1   # one per segment + colour row
    assert rows[-1][0] == "Colour"


def test_sides_session_left_weekly_right():
    sides = {s["source"]: s["side"] for s in Dial.SEGMENTS}
    assert sides["session"] == "left"
    assert sides["weekly"] == "right"


def test_health_color_endpoints():
    assert Dial._health_color(1.0) == "rgb(48,209,88)"   # green
    assert Dial._health_color(0.0) == "rgb(255,69,58)"   # red


def test_render_returns_coloured_image():
    img = Dial.render(BLANK_LIMITS, mark_color="rgb(1,2,3)")
    assert (img.size().width, img.size().height) == (18, 18)
    assert img.isTemplate() is False   # coloured, not a template
