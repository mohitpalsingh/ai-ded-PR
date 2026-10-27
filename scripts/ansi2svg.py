#!/usr/bin/env python3
"""Convert a colored terminal capture (tmux capture-pane -e) into an SVG image.

Usage:
    tmux capture-pane -p -e > /tmp/review.ansi
    python3 scripts/ansi2svg.py /tmp/review.ansi docs/screenshots/review.svg \
        --title "ai-ded-pr · internal/auth/token.go"

Requires only Python 3 (no third-party packages). The SVG is self-contained:
inline colors, monospace font, rounded terminal window.
"""
import argparse
import html
import re

SGR = re.compile(r"\x1b\[([0-9;]*)m")

# xterm-256 palette (the subset terminals use for the app's colors).
BASE16 = [
    "#000000", "#800000", "#008000", "#808000", "#000080", "#800080", "#008080", "#c0c0c0",
    "#808080", "#ff0000", "#00ff00", "#ffff00", "#0000ff", "#ff00ff", "#00ffff", "#ffffff",
]
CUBE = [0, 95, 135, 175, 215, 255]


def color_256(n: int) -> str:
    if n < 16:
        return BASE16[n]
    if n < 232:
        n -= 16
        return "#%02x%02x%02x" % (CUBE[n // 36], CUBE[(n // 6) % 6], CUBE[n % 6])
    v = 8 + (n - 232) * 10
    return "#%02x%02x%02x" % (v, v, v)


def parse_ansi(text: str):
    """Return rows of (text, color, bold) runs."""
    rows = []
    runes = []
    fg, bold = None, False
    i = 0
    while i < len(text):
        m = SGR.match(text, i)
        if m:
            params = [int(p) for p in m.group(1).split(";") if p != ""] or [0]
            j = 0
            while j < len(params):
                p = params[j]
                if p == 0:
                    fg, bold = None, False
                elif p == 1:
                    bold = True
                elif p == 22:
                    bold = False
                elif p == 39:
                    fg = None
                elif 30 <= p <= 37:
                    fg = BASE16[p - 30]
                elif 90 <= p <= 97:
                    fg = BASE16[p - 90 + 8]
                elif p == 38 and j + 2 < len(params) and params[j + 1] == 5:
                    fg = color_256(params[j + 2])
                    j += 2
                elif p == 38 and j + 4 < len(params) and params[j + 1] == 2:
                    fg = "#%02x%02x%02x" % tuple(params[j + 2:j + 5])
                    j += 4
                j += 1
            i = m.end()
            continue
        ch = text[i]
        i += 1
        if ch == "\n":
            rows.append(runes)
            runes = []
        elif ch not in "\r":
            runes.append((ch, fg, bold))
    if runes:
        rows.append(runes)
    return rows


def svg_escape(s: str) -> str:
    return html.escape(s, quote=False)


def build_svg(rows, title="ai-ded-pr"):
    cols = max((len(r) for r in rows), default=80)
    font = 13
    char_w = font * 0.6023  # Menlo/SF Mono advance
    line_h = 18
    pad = 20
    bar_h = 34
    width = int(pad * 2 + cols * char_w)
    height = int(bar_h + pad * 2 + len(rows) * line_h)

    out = []
    out.append(
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" font-family="ui-monospace, SFMono-Regular, '
        f'Menlo, Consolas, monospace" font-size="{font}">'
    )
    out.append(
        f'<defs><clipPath id="win"><rect x="0.5" y="0.5" width="{width - 1}" '
        f'height="{height - 1}" rx="10"/></clipPath></defs>'
    )
    out.append(f'<g clip-path="url(#win)">')
    out.append(f'<rect x="0" y="0" width="{width}" height="{height}" fill="#1e1e2e"/>')
    out.append(f'<rect x="0" y="0" width="{width}" height="{bar_h}" fill="#181825"/>')
    out.append(f'<line x1="0" y1="{bar_h}" x2="{width}" y2="{bar_h}" stroke="#313244"/>')
    for k, c in enumerate(("#f38ba8", "#f9e2af", "#a6e3a1")):
        out.append(f'<circle cx="{pad + k * 18}" cy="{bar_h / 2}" r="5" fill="{c}"/>')
    out.append(
        f'<text x="{width / 2}" y="{bar_h / 2 + 4}" fill="#7f849c" font-size="12" '
        f'text-anchor="middle" xml:space="preserve">{svg_escape(title)}</text>'
    )
    out.append("</g>")
    out.append(f'<rect x="0.5" y="0.5" width="{width - 1}" height="{height - 1}" rx="10" '
               f'fill="none" stroke="#313244"/>')

    out.append(f'<g xml:space="preserve">')
    for r, row in enumerate(rows):
        y = bar_h + pad + (r + 1) * line_h - 5
        # collapse runs of identical styling
        parts = []
        cur, cur_fg, cur_bold = [], None, False

        def flush():
            if not cur:
                return
            text = svg_escape("".join(cur))
            attrs = ""
            if cur_fg:
                attrs += f' fill="{cur_fg}"'
            if cur_bold:
                attrs += ' font-weight="bold"'
            parts.append(f"<tspan{attrs}>{text}</tspan>")

        for ch, fg, bold in row:
            if fg != cur_fg or bold != cur_bold:
                flush()
                cur, cur_fg, cur_bold = [], fg, bold
            cur.append(ch)
        flush()
        line = "".join(parts).rstrip()
        if line:
            out.append(f'<text x="{pad}" y="{y}" fill="#cdd6f4" xml:space="preserve">{line}</text>')
    out.append("</g></svg>\n")
    return "\n".join(out)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("input", help="ANSI capture from tmux capture-pane -e")
    ap.add_argument("output", help="SVG path to write")
    ap.add_argument("--title", default="ai-ded-pr")
    args = ap.parse_args()

    with open(args.input, encoding="utf-8", errors="replace") as f:
        text = f.read().rstrip("\n")
    rows = parse_ansi(text)
    svg = build_svg(rows, args.title)
    with open(args.output, "w", encoding="utf-8") as f:
        f.write(svg)
    print(f"wrote {args.output} ({len(rows)} rows)")


if __name__ == "__main__":
    main()
