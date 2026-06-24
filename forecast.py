"""Burn-rate forecast and the consumption-rate tracker (no GUI deps)."""

from collections import deque
from dataclasses import dataclass
from typing import Optional

from helpers import fmt_remaining


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
            return None   # not enough history yet — keep the previous rate
        return (max(0.0, (sess_pct - base[1]) / dt),
                max(0.0, (week_pct - base[2]) / dt))
