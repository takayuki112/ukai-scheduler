"""Draws story-light.svg and story-dark.svg. The steps are checked by TestDocumentedStory."""
import os

W, H = 880, 468
X0, CELL, GAP = 216, 80, 6  # where the GPUs begin, and their size
GPU_Y, LANE_Y, JOB_H = 132, {"alice": 252, "bob": 346}, 72
THEMES = {
    "light": dict(bg="#ffffff", fg="#1f2328", dim="#59636e", line="#d1d9e0", slot="#f6f8fa", budget="#0969da",
                  promoted="#8250df", excess="#9a6700", held="#cf222e"),
    "dark": dict(bg="#0d1117", fg="#f0f6fc", dim="#9198a1", line="#3d444d", slot="#151b23", budget="#1f6feb",
                 promoted="#8957e5", excess="#9e6a03", held="#da3633"),
}
MOVE, FADE = 1.1, 0.6  # seconds

STEPS = [  # title, explanation, length in seconds
    ("Alice starts a P2 job on 4 GPUs", "Alice's budget is unused, so the running job is promoted to P1 priority.", 7),
    ("Bob starts a P1 job on 4 GPUs", "It fits Bob's budget of 4.", 5.5),
    ("Bob submits another P1 job", "Bob's budget is used up, so it is held. It evicts nobody.", 6),
    ("Alice submits a P1 job on 4 GPUs", "It takes Alice's budget back: Alice's own P2 is demoted, and makes room.", 10.5),
    ("Bob's first job ends", "Now Bob's second P1 fits the budget, and starts. 2 free GPUs are too few for train.", 8),
    ("Bob's second job ends too", "Idle GPUs go to anyone: Alice's P2 runs again, as excess.", 8),
]
TOTAL = sum(s[2] for s in STEPS)
BEGIN = [sum(s[2] for s in STEPS[:i]) for i in range(len(STEPS))]


def at(step, offset):
    return BEGIN[step - 1] + offset


class Track:
    """A property over time, by (end of change, value). It holds between changes."""

    def __init__(self, value):
        self.keys = [(0, value)]

    def to(self, start, value, length=FADE):
        self.keys += [(start, self.keys[-1][1]), (start + length, value)]
        return self


class Drawing:
    def __init__(self, theme):
        self.theme, self.css, self.body, self.n = theme, [], [], 0

    def animate(self, prop, track, easing=""):
        """Returns a class. Each property needs an element of its own."""
        if len(track.keys) == 1:
            return f'style="{prop}:{track.keys[0][1]}"'
        self.n += 1
        frames = "".join(f"{100 * t / TOTAL:.3f}%{{{prop}:{v}}}" for t, v in track.keys + [(TOTAL, track.keys[-1][1])])
        self.css.append(f"@keyframes k{self.n}{{{frames}}}.a{self.n}{{animation-name:k{self.n}{easing}}}")
        return f'class="a a{self.n}"'

    def add(self, s):
        self.body.append(s)

    def text(self, x, y, s, size=15, color="fg", bold=False, anchor="start"):
        fill = self.theme.get(color, color)
        self.add(f'<text x="{x}" y="{y}" font-size="{size}" fill="{fill}" text-anchor="{anchor}"'
                 f'{" font-weight=" + chr(34) + "600" + chr(34) if bold else ""}>{s}</text>')

    def shown(self, spans, draw):
        """Draws something that is visible during spans of (start, end)."""
        track = Track(0)
        for start, end in spans:
            track.to(start, 1, 0.4)
            if end is not None:
                track.to(end - 0.3, 0, 0.3)
        self.add(f"<g {self.animate('opacity', track)}>")
        draw()
        self.add("</g>")


def place(col, where):
    y = GPU_Y if where == "gpu" else LANE_Y[where]
    return f"translate({X0 + col * CELL}px,{y}px)"


class Job:
    def __init__(self, d, name, gpus):
        self.d, self.name, self.gpus, self.labels = d, name, gpus, []

    def draw(self, pos, opacity, fill):
        d, w = self.d, self.gpus * CELL - GAP
        d.add(f"<g {d.animate('transform', pos)}><g {d.animate('opacity', opacity)}>")
        d.add(f'<rect x="-1" y="-1" width="{w + 2}" height="{JOB_H + 2}" rx="9" {d.animate("fill", fill)}/>')
        d.text(14, 31, self.name, 19, "#ffffff", True)
        for start, end, label in self.labels:
            d.shown([(start, end)], lambda: d.text(14, 55, label, 15, "#ffffff"))
        d.add("</g></g>")


def draw(theme):
    c = THEMES[theme]
    d = Drawing(c)
    # Header: step, title, explanation, progress.
    for i, (title, text, length) in enumerate(STEPS):
        def header():
            d.text(30, 34, f"Step {i + 1} of {len(STEPS)}", 15, "dim", True)
            d.text(30, 68, title, 26, "fg", True)
            d.text(30, 97, text, 18, "dim")
        d.shown([(BEGIN[i], BEGIN[i] + length)], header)
        x, w = 560 + i * 49, 43
        d.add(f'<rect x="{x}" y="26" width="{w}" height="6" rx="3" fill="{c["line"]}"/>')
        bar = Track("scaleX(0)").to(BEGIN[i], "scaleX(1)", length)
        d.add(f'<g transform="translate({x} 26)"><rect width="{w}" height="6" rx="3" fill="{c["fg"]}" {d.animate("transform", bar, ";animation-timing-function:linear!important")}/></g>')

    # GPUs and the lanes where jobs wait.
    d.text(30, GPU_Y + 32, "8 GPUs", 19, bold=True)
    d.text(30, GPU_Y + 55, "running", 15, "dim")
    for i in range(8):
        d.add(f'<rect x="{X0 + i * CELL}" y="{GPU_Y}" width="{CELL - GAP}" height="{JOB_H}" rx="8" fill="{c["slot"]}" stroke="{c["line"]}" stroke-width="1.5"/>')
    d.text(X0, 238, "Waiting", 15, "dim", True)
    for user, y in LANE_Y.items():
        d.add(f'<rect x="{X0 - 6}" y="{y - 6}" width="{8 * CELL + 6}" height="{JOB_H + 12}" rx="10" fill="none" stroke="{c["line"]}" stroke-width="1.5" stroke-dasharray="5 5"/>')
        d.text(30, y + 18, user.capitalize(), 19, bold=True)

    # Times of what happens.
    t1, t2, t3, t4, t5, t6 = (at(i, 0) for i in range(1, 7))
    train_up, eval_up, debug_up, eval2_up, train_back = t1 + 2, t2 + 1.8, t4 + 5.8, t5 + 4.2, t6 + 4
    demoted, eval_ends, eval2_ends, end = t4 + 2.4, t5 + 1.2, t6 + 1.2, TOTAL - 0.7
    promoted = train_up + MOVE + 0.3

    # Budget meters: four units each.
    meters = {
        "alice": ([(promoted, "promoted"), (demoted, None), (debug_up + MOVE, "budget")], {}, [
            (0.2, promoted, "budget: 0 of 4 used"), (promoted, demoted, "budget: lent to train"),
            (demoted, debug_up + MOVE, "budget: kept for debug"), (debug_up + MOVE, end + .7, "budget: 4 of 4 used")]),
        "bob": ([(eval_up + MOVE, "budget"), (eval_ends, None)], {0: 1, 1: 1}, [
            (0.2, eval_up + MOVE, "budget: 0 of 4 used"), (eval_up + MOVE, eval_ends, "budget: 4 of 4 used"),
            (eval_ends, eval2_up + MOVE, "budget: 0 of 4 used"), (eval2_up + MOVE, eval2_ends, "budget: 2 of 4 used"),
            (eval2_ends, end + .7, "budget: 0 of 4 used")]),
    }
    for user, (changes, second, texts) in meters.items():
        y = LANE_Y[user]
        for i in range(4):
            fill = Track(c["slot"])
            for when, tier in changes:
                fill.to(when, c[tier] if tier else c["slot"])
            if i in second:
                fill.to(eval2_up + MOVE, c["budget"]).to(eval2_ends, c["slot"])
            fill.to(end, c["slot"], 0.6)
            d.add(f'<rect x="{30 + i * 37}" y="{y + 29}" width="33" height="14" rx="4" stroke="{c["line"]}" stroke-width="1.5" {d.animate("fill", fill)}/>')
        for start, stop, s in texts:
            d.shown([(start, stop)], lambda: d.text(30, y + 63, s, 15, "dim"))

    def appear(start, stop):
        return Track(0).to(start, 1, 0.5).to(stop, 0, 0.7)

    ev = Job(d, "Bob: eval", 4)
    ev.labels = [(t2, None, "P1, within budget, priority 75")]
    ev.draw(Track(place(0, "bob")).to(eval_up, place(4, "gpu"), MOVE), appear(t2 + .4, eval_ends), Track(c["budget"]))

    # Demoted at the far side of the lane, so that the two do not cross.
    train = Job(d, "Alice: train", 4)
    out = demoted + 1.6
    train.labels = [(t1, promoted, "P2, beyond the allowance of 2"), (promoted, demoted, "P2, promoted, priority 75"),
                    (demoted, None, "P2, excess, priority 45")]
    train.draw(Track(place(0, "alice")).to(train_up, place(0, "gpu"), MOVE).to(out, place(4, "alice"), MOVE)
               .to(train_back, place(4, "gpu"), MOVE),
               appear(t1 + .4, end), Track(c["excess"]).to(promoted, c["promoted"]).to(demoted, c["excess"]))

    debug = Job(d, "Alice: debug", 4)
    debug.labels = [(t4, None, "P1, within budget, priority 75")]
    debug.draw(Track(place(0, "alice")).to(debug_up, place(0, "gpu"), MOVE), appear(t4 + .8, end), Track(c["budget"]))

    # Last, as it passes over train.
    ev2 = Job(d, "Bob: eval2", 2)
    ev2.labels = [(t3, eval_ends + 1, "P1, held"), (eval_ends + 1, None, "P1, in budget")]
    ev2.draw(Track(place(4, "bob")).to(eval2_up, place(4, "gpu"), MOVE), appear(t3 + 1, eval2_ends),
             Track(c["held"]).to(eval_ends + 1, c["budget"]))

    x = 30
    for tier, note, width in (("budget", "P1 within budget", 176), ("promoted", "P2 promoted: as safe as P1", 252),
                              ("excess", "P2 excess: gives way first", 238), ("held", "P1 held", 0)):
        d.add(f'<rect x="{x}" y="439" width="16" height="16" rx="4" fill="{c[tier]}"/>')
        d.text(x + 24, 452, note, 15, "dim")
        x += width

    # Without motion: the point of step 4 where the demoted job has yet to leave.
    still = demoted + 1.2
    style = (f"text{{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif}}"
             f".a{{animation:{TOTAL}s cubic-bezier(.4,0,.2,1) infinite}}" + "".join(d.css) +
             f"@media(prefers-reduced-motion:reduce){{.a{{animation-play-state:paused!important;animation-delay:-{still}s!important}}}}")
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" role="img">'
            f"<title>How jobs of two people share 8 GPUs, in six steps</title><style>{style}</style>"
            f'<rect width="{W}" height="{H}" rx="10" fill="{c["bg"]}"/>' + "".join(d.body) + "</svg>\n")


def save(name, svg):
    with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), name), "w") as f:
        f.write(svg)


if __name__ == "__main__":
    for name in THEMES:
        save(f"story-{name}.svg", draw(name))
