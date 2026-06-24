"""The menu-bar dial: an SVG rendered to a coloured NSImage."""

import math

from AppKit import NSImage
from Foundation import NSData

# a limit is {"pct": used 0..100, "time_left": fraction of window remaining 0..1}
BLANK_LIMITS = {"session": {"pct": 100.0, "time_left": 1.0},
                "weekly": {"pct": 100.0, "time_left": 1.0}}


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
    #   time_elapsed   time since last reset elapsed (fills + GREENS toward reset:
    #                  the less time left in the window, the greener)
    SEGMENTS = [
        dict(ring="outer", side="left",  source="session", metric="percent_left", bright=True,  label="Session left"),
        dict(ring="inner", side="left",  source="session", metric="time_elapsed", bright=False, label="Session since reset"),
        dict(ring="outer", side="right", source="weekly",  metric="percent_left", bright=True,  label="Weekly left"),
        dict(ring="inner", side="right", source="weekly",  metric="time_elapsed", bright=False, label="Weekly since reset"),
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
            "time_elapsed": (1.0 - t_left, 1.0 - t_left),
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
