"""Draws the other diagrams, in light and dark, as GitHub adds controls to Mermaid's."""
from story import THEMES, save

FONT = "-apple-system,BlinkMacSystemFont,'Segoe UI','Noto Sans',Helvetica,Arial,sans-serif"
LINE = 20  # height


class Diagram:
    def __init__(self, theme, width, height, title):
        self.c, self.body = THEMES[theme], []
        self.head = (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" width="{width}" height="{height}" role="img">'
                     f'<title>{title}</title><style>text{{font-family:{FONT};font-size:15px}}</style>'
                     f'<rect width="{width}" height="{height}" rx="10" fill="{self.c["bg"]}"/>')

    def text(self, x, y, s, color="fg", anchor="middle", bold=False):
        weight = ' font-weight="600"' if bold else ""
        self.body.append(f'<text x="{x}" y="{y}" fill="{self.c.get(color, color)}" text-anchor="{anchor}"{weight}>{s}</text>')

    def box(self, x, y, w, *lines, kind="step", bold=0):
        """A box around the point x, y. The first lines can be bold; kind is a tier, or how it looks."""
        h = LINE * len(lines) + 18
        fill, stroke, color, extra = self.c["slot"], self.c["line"], "fg", ""
        if kind in ("budget", "promoted", "excess", "held"):
            fill = stroke = self.c[kind]
            color = "#ffffff"
        elif kind == "question":
            stroke, extra = self.c["dim"], ' stroke-dasharray="5 4"'
        rx = h / 2 if kind == "end" else 8
        self.body.append(f'<rect x="{x - w / 2:g}" y="{y - h / 2:g}" width="{w}" height="{h}" rx="{rx:g}" fill="{fill}" stroke="{stroke}" stroke-width="1.5"{extra}/>')
        for i, s in enumerate(lines):
            self.text(x, f"{y - LINE * (len(lines) - 1) / 2 + i * LINE + 5:g}", s, color, bold=i < bold)

    def group(self, x, y, w, h, title):
        self.body.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="10" fill="none" stroke="{self.c["dim"]}" stroke-width="1.5"/>')
        if title:
            self.text(x + 14, y + 24, title, "dim", "start", True)

    def dot(self, x, y, ring=False):
        if ring:
            self.body.append(f'<circle cx="{x}" cy="{y}" r="11" fill="none" stroke="{self.c["dim"]}" stroke-width="1.5"/>')
        self.body.append(f'<circle cx="{x}" cy="{y}" r="7" fill="{self.c["dim"]}"/>')

    def line(self, *points, dashed=False, arrow=True, color="dim"):
        (x0, y0), (x, y) = points[-2:]
        dx, dy = (x > x0) - (x < x0), (y > y0) - (y < y0)  # all lines are straight
        extra = ' stroke-dasharray="5 4"' if dashed else ""
        path = " ".join(f"{a},{b}" for a, b in points)
        self.body.append(f'<polyline points="{path}" fill="none" stroke="{self.c[color]}" stroke-width="1.5"{extra}/>')
        if arrow:
            ax, ay = x - 8 * dx, y - 8 * dy
            self.body.append(f'<path d="M{x},{y} L{ax + 5 * dy},{ay + 5 * dx} L{ax - 5 * dy},{ay - 5 * dx} z" fill="{self.c["dim"]}"/>')

    def svg(self):
        return self.head + "".join(self.body) + "</svg>\n"


def overview(theme):
    d = Diagram(theme, 600, 330, "The scheduler, every cycle")
    d.group(20, 78, 560, 128, "Scheduler, every cycle")
    d.box(92, 34, 130, "Jobs and pods", kind="end")
    d.box(92, 150, 110, "Snapshot")
    d.box(276, 150, 180, "tierbudget", "who is over budget,", "promoted or excess?", bold=1, kind="budget")
    d.box(482, 150, 160, "KAI's actions", "allocate, preempt, ...")
    d.box(276, 284, 170, "Metrics, /tiers,", "events on held pods")
    d.box(482, 284, 100, "GPUs", kind="end")
    d.line((92, 53), (92, 78))
    d.line((147, 150), (186, 150))
    d.line((366, 150), (402, 150))
    d.line((276, 189), (276, 255), dashed=True)
    d.line((482, 206), (482, 265))
    return d


def tiers(theme):
    d = Diagram(theme, 700, 630, "How a job gets its tier")
    d.box(350, 32, 80, "Job", kind="end")
    d.box(350, 118, 190, "Has GPUs, and its", "class is listed?", kind="question")
    d.box(602, 118, 140, "unmanaged", "as in stock KAI", bold=1)
    d.box(350, 208, 90, "Class", kind="question")
    d.line((350, 51), (350, 89))
    d.line((445, 118), (532, 118)); d.text(488, 110, "no", "dim")
    d.line((350, 147), (350, 189)); d.text(360, 173, "yes", "dim", "start")
    d.line((305, 208), (80, 208), (80, 281)); d.text(88, 252, "charged", "dim", "start")
    d.line((350, 227), (350, 240), (270, 240), (270, 281)); d.text(278, 262, "budgeted", "dim", "start")
    d.line((395, 208), (540, 208), (540, 281)); d.text(548, 252, "promotable", "dim", "start")
    d.box(80, 300, 100, "budget", kind="budget", bold=1)
    d.box(270, 300, 110, "Running?", kind="question")
    d.box(80, 400, 100, "budget", kind="budget", bold=1)
    d.line((215, 300), (170, 300), (170, 400), (130, 400)); d.text(178, 358, "yes", "dim", "start")
    d.box(270, 400, 180, "Fits the budget, after", "what the user runs?", kind="question")
    d.line((270, 319), (270, 371)); d.text(280, 350, "no", "dim", "start")
    d.box(150, 510, 110, "budget", "may start", kind="budget", bold=1)
    d.box(310, 510, 130, "held", "waits, says why", kind="held", bold=1)
    d.line((220, 429), (220, 450), (150, 450), (150, 481)); d.text(158, 472, "yes", "dim", "start")
    d.line((310, 429), (310, 481)); d.text(318, 472, "no", "dim", "start")
    d.box(540, 300, 110, "Running?", kind="question")
    d.box(470, 400, 160, "Fits what is left", "of the budget?", kind="question")
    d.line((500, 319), (500, 371)); d.text(492, 350, "yes", "dim", "end")
    d.box(450, 510, 110, "promoted", "P1 priority", kind="promoted", bold=1)
    d.line((430, 429), (430, 481)); d.text(438, 472, "yes", "dim", "start")
    d.box(600, 495, 130, "Fits the", "allowance?", kind="question")
    d.line((550, 410), (580, 410), (580, 466)); d.text(588, 440, "no", "dim", "start")
    d.line((595, 300), (640, 300), (640, 466)); d.text(632, 350, "no", "dim", "end")
    d.box(480, 594, 160, "normal", "keeps its priority", bold=1)
    d.box(630, 594, 120, "excess", "lower priority", kind="excess", bold=1)
    d.line((565, 524), (565, 544), (530, 544), (530, 565)); d.text(522, 558, "yes", "dim", "end")
    d.line((635, 524), (635, 565)); d.text(643, 550, "no", "dim", "start")
    return d


def components(theme):
    d = Diagram(theme, 620, 420, "The parts and how they call each other")
    d.box(165, 48, 270, "cmd/scheduler", "registers both, then runs KAI", bold=1)
    d.box(470, 48, 240, "cmd/replay, pkg/replay", "one cycle on a snapshot", bold=1)
    d.box(310, 144, 210, "KAI v0.18.9, unmodified")
    d.line((245, 77), (245, 125))
    d.line((375, 77), (375, 125))
    d.box(155, 260, 270, "pkg/tierbudget", "plugin: reads jobs, applies results", kind="budget", bold=1)
    d.box(480, 260, 170, "ukai-consolidation", "wraps KAI's", bold=1)
    d.line((245, 163), (245, 231)); d.text(237, 203, "each cycle", "dim", "end")
    d.line((405, 163), (405, 196), (480, 196), (480, 231)); d.text(488, 218, "each cycle", "dim", "start")
    d.line((395, 260), (290, 260))
    d.box(155, 370, 220, "pkg/tiers", "the rules, a pure function", bold=1)
    d.line((155, 289), (155, 341))
    return d


def cycle(theme):
    d = Diagram(theme, 700, 560, "One cycle, as a sequence of calls")
    k, p, t = 85, 390, 625
    for x, name in ((k, "KAI"), (p, "tierbudget"), (t, "tiers.Assign")):
        d.line((x, 45), (x, 545), arrow=False, color="line")
        d.box(x, 30, 120, name, bold=1)

    def call(a, b, y, s, dashed=False):
        d.line((a, y), (b, y), dashed=dashed)
        d.text((a + b) // 2, y - 8, s)

    def own(x, y, *lines):
        d.line((x, y - 12), (x + 24, y - 12), (x + 24, y + 12), (x, y + 12))
        for i, s in enumerate(lines):
            d.text(x + 34, f"{y + 5 - LINE * (len(lines) - 1) / 2 + i * LINE:g}", s, anchor="start")

    own(k, 80, "snapshot of the cluster")
    call(k, p, 134, "OnSessionOpen")
    d.text((p + t) // 2, 160, "jobs: user, class,")
    call(p, t, 188, "units, running, times")
    call(t, p, 226, "tier and priority of each", True)
    own(p, 262, "publish metrics and status")
    d.text((k + p) // 2, 306, "in enforce mode: set in-memory")
    call(p, k, 334, "priorities, add hooks")
    d.group(45, 356, 385, 128, "")
    d.text((k + p) // 2, 376, "loop: allocate, consolidate,", "dim", bold=True)
    d.text((k + p) // 2, 396, "reclaim, preempt", "dim", bold=True)
    call(k, p, 432, "may this task be placed?")
    call(p, k, 470, "no, with a reason, if over budget now", True)
    call(k, p, 524, "OnSessionClose")
    return d


def install(theme):
    d = Diagram(theme, 700, 260, "The steps of an installation, and the ways back")
    for x, lines in ((75, ("1 Replay", "offline")), (212, ("2 Image",)), (350, ("3 Audit", "only logs")),
                     (488, ("4 Policies",)), (625, ("5 Enforce",))):
        d.box(x, 100, 110, *lines, bold=1)
    for x in (130, 267, 405, 543):
        d.line((x, 100), (x + 27, 100))
    d.line((625, 81), (625, 34), (350, 34), (350, 71), dashed=True); d.text(488, 26, "mode: audit", "dim")
    d.box(488, 222, 120, "Stock KAI")
    d.line((350, 129), (350, 222), (428, 222), dashed=True); d.text(342, 180, "stock values", "dim", "end")
    d.line((625, 119), (625, 222), (548, 222), dashed=True); d.text(617, 180, "stock values", "dim", "end")
    return d


def which(theme):
    d = Diagram(theme, 600, 310, "Which class to choose")
    d.box(150, 52, 190, "Need it soon, and", "is it small?", kind="question")
    d.box(450, 52, 230, "P1", "counts against your budget", kind="budget", bold=1)
    d.box(150, 158, 190, "Would losing progress", "hurt?", kind="question")
    d.box(450, 158, 230, "P2", "promoted while your", "budget is unused", kind="promoted", bold=1)
    d.box(150, 264, 190, "P3", "whatever is free", bold=1)
    for y in (52, 158):
        d.line((245, y), (335, y)); d.text(290, y - 8, "yes", "dim")
        d.line((150, y + 29), (150, y + 77)); d.text(160, y + 57, "no", "dim", "start")
    return d


def queue(theme):
    d = Diagram(theme, 600, 370, "A P1 job with the planned order")
    d.dot(110, 30)
    d.line((117, 30), (490, 30), (490, 71)); d.text(300, 22, "submitted, budget in use", "dim")
    d.line((110, 37), (110, 151)); d.text(120, 98, "submitted, fits", "dim", "start")
    d.box(490, 90, 110, "Held", kind="held", bold=1)
    d.box(110, 170, 110, "In line", bold=1)
    d.line((490, 109), (490, 170), (165, 170)); d.text(325, 162, "the budget has room, the time is noted", "dim")
    d.line((110, 189), (110, 241)); d.text(120, 220, "GPUs are free", "dim", "start")
    d.box(110, 260, 110, "Running", kind="budget", bold=1)
    d.line((110, 279), (110, 324))
    d.dot(110, 336, ring=True)
    return d


if __name__ == "__main__":
    for draw in (overview, tiers, components, cycle, install, which, queue):
        for theme in THEMES:
            save(f"{draw.__name__}-{theme}.svg", draw(theme).svg())
