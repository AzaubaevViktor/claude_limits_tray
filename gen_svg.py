"""Generate a large standalone SVG of the dial for the README.

Reuses the real Dial geometry/colours so the picture can't drift from the app.
Run: .venv/bin/python gen_svg.py  ->  writes dial.svg
"""

from dial import Dial


def svg(limits, mark_color, size=240, bg="#1c1c1e"):
    cx = Dial.CX
    span = 180.0 - 2 * Dial.END_GAP
    p = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{size}" '
         f'height="{size}" viewBox="0 0 18 18">']
    if bg:
        p.append(f'<rect x="0" y="0" width="18" height="18" rx="4" fill="{bg}"/>')

    for sign in Dial.SIDE_SIGN.values():
        ox = cx + sign * Dial.HALF_SHIFT
        for r in Dial.RINGS.values():
            d = Dial._arc(ox, r, sign * Dial.END_GAP, sign * (180.0 - Dial.END_GAP))
            p.append(f'<path d="{d}" fill="none" stroke="{Dial.TRACK}" '
                     f'stroke-width="{Dial.SW}" stroke-opacity="0.45" '
                     f'stroke-linecap="round"/>')

    for seg in Dial.SEGMENTS:
        length, health = Dial._metric(seg["metric"], limits[seg["source"]])
        length = max(0.0, min(1.0, length))
        if length <= 0.004:
            continue
        sign = Dial.SIDE_SIGN[seg["side"]]
        r = Dial.RINGS[seg["ring"]]
        ox = cx + sign * Dial.HALF_SHIFT
        start = sign * Dial.END_GAP
        end = sign * (Dial.END_GAP + length * span)
        alpha = 1.0 if seg["bright"] else 0.85
        p.append(f'<path d="{Dial._arc(ox, r, start, end)}" fill="none" '
                 f'stroke="{Dial._health_color(health)}" stroke-width="{Dial.SW}" '
                 f'stroke-linecap="round" stroke-opacity="{alpha}"/>')

    p.append(Dial._mark(mark_color))
    p.append("</svg>")
    return "".join(p)


if __name__ == "__main__":
    # a relaxed, good-looking sample: lots of quota left, mid-window
    limits = {
        "session": {"pct": 22.0, "time_left": 0.35},   # 78% left, 65% elapsed
        "weekly": {"pct": 10.0, "time_left": 0.60},    # 90% left, 40% elapsed
    }
    with open("dial.svg", "w") as f:
        f.write(svg(limits, mark_color="rgb(175,82,222)"))   # purple = relaxed
    print("wrote dial.svg")
