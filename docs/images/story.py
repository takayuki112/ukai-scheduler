"""Draws story.gif. Needs Pillow. The steps are checked by TestDocumentedStory."""
import os
from PIL import Image, ImageDraw, ImageFont

W, H, S = 900, 430, 2
BG, FG, DIM, LINE = "#ffffff", "#1f2328", "#59636e", "#d1d9e0"
TIER = {"budget": "#0969da", "promoted": "#8250df", "normal": "#1a7f37", "excess": "#bf8700", "held": "#cf222e", "waiting": "#59636e"}


def font(size, bold=False):
    for path, index in [("/System/Library/Fonts/Helvetica.ttc", 1 if bold else 0),
                        ("/usr/share/fonts/truetype/dejavu/DejaVuSans%s.ttf" % ("-Bold" if bold else ""), 0)]:
        if os.path.exists(path):
            return ImageFont.truetype(path, size * S, index=index)
    return ImageFont.load_default(size * S)


# title, explanation, running [(name, gpus, tier, priority)], queue [(name, gpus, tier, note)]
STEPS = [
    ("1. Alice starts a P2 on 4 GPUs", "Alice's budget of 4 is unused, so the job is promoted to P1 priority.",
     [("alice / train", 4, "promoted", "P2, promoted to 75")], []),
    ("2. Bob starts a P1 on 4 GPUs", "It fits Bob's budget of 4.",
     [("alice / train", 4, "promoted", "P2, promoted to 75"), ("bob / eval", 4, "budget", "P1, 75")], []),
    ("3. Bob submits another P1", "Bob's budget is used up, so it waits. It evicts nobody.",
     [("alice / train", 4, "promoted", "P2, promoted to 75"), ("bob / eval", 4, "budget", "P1, 75")],
     [("bob / eval2", 2, "held", "owner budget: needs 2 units, using 4 of 4")]),
    ("4. Alice submits a P1 on 4 GPUs", "It needs Alice's budget, so Alice's own P2 is demoted and makes room.",
     [("alice / debug", 4, "budget", "P1, 75"), ("bob / eval", 4, "budget", "P1, 75")],
     [("bob / eval2", 2, "held", "owner budget: needs 2 units, using 4 of 4"), ("alice / train", 4, "excess", "P2 beyond the allowance, 45")]),
    ("5. Bob's first P1 ends", "The second one fits Bob's budget now, and starts.",
     [("alice / debug", 4, "budget", "P1, 75"), ("bob / eval2", 2, "budget", "P1, 75")],
     [("alice / train", 4, "excess", "waits for 4 free GPUs")]),
    ("6. That ends too", "Idle GPUs go to anyone: Alice's P2 runs again, as excess.",
     [("alice / debug", 4, "budget", "P1, 75"), ("alice / train", 4, "excess", "P2 beyond the allowance, 45")], []),
]


def frame(step):
    title, text, running, queue = STEPS[step]
    im = Image.new("RGB", (W * S, H * S), BG)
    d = ImageDraw.Draw(im)

    def t(x, y, s, size=14, color=FG, bold=False):
        d.text((x * S, y * S), s, font=font(size, bold), fill=color)

    def box(x, y, w, h, **kw):
        d.rounded_rectangle((x * S, y * S, (x + w) * S, (y + h) * S), radius=6 * S, **kw)

    t(30, 22, title, 24, bold=True)
    t(30, 58, text, 16, DIM)
    t(30, 102, "8 GPUs", 14, DIM, True)
    x, cell = 30, 105
    for i in range(8):
        box(x + i * cell, 124, cell - 6, 92, outline=LINE, width=2 * S)
    for name, n, tier, note in running:
        box(x, 124, n * cell - 6, 92, fill=TIER[tier])
        t(x + 14, 140, name, 17, "#ffffff", True)
        t(x + 14, 170, note, 14, "#ffffff")
        x += n * cell
    t(30, 240, "Waiting", 14, DIM, True)
    y = 264
    for name, n, tier, note in queue:
        box(30, y, 834, 42, outline=TIER[tier], width=2 * S)
        box(30, y, 10, 42, fill=TIER[tier])
        t(54, y + 11, name, 15, FG, True)
        t(180, y + 12, f"{n} GPUs", 14, DIM)
        t(260, y + 12, f"{tier}: {note}", 14, TIER[tier])
        y += 50
    if not queue:
        t(30, 268, "nothing", 15, DIM)
    x = 30
    for tier in ("budget", "promoted", "excess", "held"):
        box(x, 388, 16, 16, fill=TIER[tier])
        t(x + 24, 387, tier, 14, DIM)
        x += 120
    for i in range(len(STEPS)):
        d.ellipse(((740 + i * 22) * S, 390 * S, (752 + i * 22) * S, 402 * S), fill=FG if i == step else LINE)
    return im.resize((W, H), Image.LANCZOS)


frames = [frame(i) for i in range(len(STEPS))]
out = os.path.join(os.path.dirname(os.path.abspath(__file__)), "story.gif")
frames = [f.quantize(96) for f in frames]
frames[0].save(out, save_all=True, append_images=frames[1:], duration=3800, loop=0, optimize=True)
if os.environ.get("STILLS"):
    for i in range(len(STEPS)):
        frame(i).save(os.path.join(os.environ["STILLS"], f"story{i + 1}.png"))
