package dashboard

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/takayuki112/ukai-scheduler/pkg/tiers"
)

// Options say how to draw.
type Options struct {
	Width  int // 0 for any
	Color  bool
	Others bool // name other users and their jobs
	Limit  int  // of jobs of others in each queue
	Now    time.Time
}

type style string

const (
	plain  style = ""
	bold   style = "1"
	dim    style = "2"
	red    style = "31"
	green  style = "32"
	yellow style = "33"
	blue   style = "34"
	purple style = "35"
)

var tierStyle = map[tiers.Tier]style{tiers.Budget: blue, tiers.Held: red, tiers.Promoted: purple, tiers.Normal: plain, tiers.Excess: yellow}

var tierMeaning = map[tiers.Tier]string{
	tiers.Budget:   "P1 and charged, within the budget",
	tiers.Held:     "wait for your own budget, even if GPUs are free",
	tiers.Promoted: "P2 at P1 priority",
	tiers.Normal:   "P2 at their own priority",
	tiers.Excess:   "P2 below their own priority, evicted first",
}

// wide is from where jobs and queues are side by side.
const wide = 160

type line struct {
	style style
	text  string
}

type canvas struct {
	Options
	lines []line
}

func (c *canvas) add(s style, format string, a ...any) {
	text := Clean(fmt.Sprintf(format, a...))
	if c.Width > 0 && len(text) > c.Width {
		text = text[:max(0, c.Width-1)] + ">"
	}
	c.lines = append(c.lines, line{s, text})
}

func (c *canvas) paint(l line) string {
	if c.Color && l.style != plain && l.text != "" {
		return "\x1b[" + string(l.style) + "m" + l.text + "\x1b[0m"
	}
	return l.text
}

// Clean keeps printable ASCII: names come from the cluster, and could hold escape
// sequences or characters of another width.
func Clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return '?'
		}
		return r
	}, s)
}

// Render draws the view, a string for each line.
func Render(v *View, o Options) []string {
	c := &canvas{Options: o}
	mode := ""
	if v.Audit {
		mode = "   AUDIT: what would be, nothing is applied"
	}
	c.add(bold, "ukai  user %s%s", v.User, mode)
	if v.Lost != "" {
		c.add(red, "NOT UP TO DATE, reconnecting: %s", v.Lost)
	}
	c.add(plain, "")
	c.budget(v)
	c.add(plain, "")
	out := []string{}
	flush := func() {
		for _, l := range c.lines {
			out = append(out, c.paint(l))
		}
		c.lines = nil
	}
	if c.Width >= wide {
		flush()
		half := o
		half.Width = (o.Width - 2) / 2
		left, right := &canvas{Options: half}, &canvas{Options: half}
		left.jobs(v)
		right.queues(v)
		for i := range max(len(left.lines), len(right.lines)) {
			var l, r line
			if i < len(left.lines) {
				l = left.lines[i]
			}
			if i < len(right.lines) {
				r = right.lines[i]
			}
			out = append(out, c.paint(l)+strings.Repeat(" ", half.Width+2-len(l.text))+c.paint(r))
		}
	} else {
		c.jobs(v)
		c.queues(v)
	}
	c.add(dim, "Tiers are computed here by the scheduler's rules, and may differ from it briefly.")
	for _, n := range v.Notes {
		c.add(dim, "! %s", n)
	}
	flush()
	return out
}

func (c *canvas) budget(v *View) {
	const width = 24
	full := 0
	if v.Budget > 0 {
		full = min(width, int(int64(width)*int64(v.Used)/int64(v.Budget)))
	}
	s := green
	if v.Used >= v.Budget {
		s = red
	}
	over := ""
	if v.Used > v.Budget {
		over = "  (over)"
	}
	approx := ""
	for _, j := range v.Jobs {
		if j.Estimated && j.Running {
			approx = "~"
		}
	}
	c.add(s, "Budget [%s%s] %s%s of %s units%s", strings.Repeat("#", full), strings.Repeat(".", width-full), approx, v.Used, v.Budget, over)
	var counts []string
	for _, t := range tierOrder {
		n := 0
		for _, j := range v.Jobs {
			if j.Tier == t {
				n++
			}
		}
		if n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, t))
		}
	}
	if len(counts) > 0 {
		c.add(plain, "Jobs   %s", strings.Join(counts, ", "))
	}
	c.types(v)
}

// types says how many GPUs of each type the user's jobs have, or wait for.
func (c *canvas) types(v *View) {
	for _, running := range []bool{true, false} {
		gpus := map[string]tiers.Units{}
		for _, j := range v.Jobs {
			if j.Running == running {
				gpus[strings.Join(slices.Sorted(slices.Values(j.Types)), "\x00")] += j.GPUs
			}
		}
		var parts []string
		for _, t := range slices.Sorted(maps.Keys(gpus)) {
			parts = append(parts, v.gpus(gpus[t], strings.Split(t, "\x00")))
		}
		if len(parts) > 0 {
			c.add(plain, "%s  %s", map[bool]string{true: "Uses ", false: "Waits"}[running], strings.Join(parts, ", "))
		}
	}
}

func (c *canvas) jobs(v *View) {
	if len(v.Jobs) == 0 {
		c.add(plain, "No jobs with GPUs in a managed class.")
		c.add(plain, "")
	}
	if len(v.Jobs) > 0 {
		c.add(dim, "  %-*s  %-16s %6s  %-7s %5s  %s", c.nameWidth(), "JOB", "GPUS", "UNITS", "STATE", "AGE", "CLASS")
	}
	for i := 0; i < len(v.Jobs); {
		tier := v.Jobs[i].Tier
		n := i
		for n < len(v.Jobs) && v.Jobs[n].Tier == tier {
			n++
		}
		c.add(bold, "%s: %s", tier, tierMeaning[tier])
		for _, j := range v.Jobs[i:n] {
			state := "waiting"
			if j.Running {
				state = "running"
			}
			if j.Gone {
				state = "ending"
			}
			c.add(tierStyle[tier], "  %-*s  %-16s %6s  %-7s %5s  %s", c.nameWidth(), cut(j.ID, c.nameWidth()), cut(v.gpus(j.GPUs, j.Types), 16), units(j), state, age(c.Now.Sub(j.Since)), j.ClassName)
			if tier == tiers.Held {
				c.add(tierStyle[tier], "    %s", j.Reason)
			}
		}
		c.add(plain, "")
		i = n
	}
}

func (c *canvas) queues(v *View) {
	if len(v.Queues) == 0 {
		c.add(bold, "No P1 is waiting.")
		c.add(plain, "")
		return
	}
	c.add(bold, "P1 waiting, oldest first")
	c.add(dim, "No promise: KAI skips jobs that do not fit, and shares GPUs between queues.")
	c.add(dim, "   # %-10s %-*s %4s %6s %5s  %s", "USER", c.nameWidth(), "JOB", "GPUS", "UNITS", "AGE", "QUEUE")
	for _, q := range v.Queues {
		c.add(bold, "%s: %s", queueName(q.Type, v.name(q.Type)), position(q, v.User))
		shown := 0
		for i, j := range q.Jobs {
			mine := j.User == v.User
			if !mine && shown >= c.Limit {
				continue
			}
			s, name, user := plain, j.ID, j.User
			if mine {
				s, user = bold, "you"
			} else {
				shown++
				if !c.Others {
					name, user = "-", "another"
				}
			}
			held := ""
			if j.Tier == tiers.Held {
				held = "  held"
			}
			c.add(s, " %3d %-10s %-*s %4s %6s %5s  %s%s", i+1, cut(user, 10), c.nameWidth(), cut(name, c.nameWidth()), j.GPUs, units(j), age(c.Now.Sub(j.Created)), j.Queue, held)
		}
		if hidden := len(q.Jobs) - shown - countOf(q, v.User); hidden > 0 {
			c.add(dim, "     and %d more of others", hidden)
		}
	}
	c.add(plain, "")
}

func queueName(t, short string) string {
	if t == anyType || t == unknownType {
		return "GPUs of " + strings.ReplaceAll(t, "type not visible", "a type that is not visible")
	}
	return short
}

func position(q Queue, user string) string {
	for i, j := range q.Jobs {
		if j.User == user {
			return fmt.Sprintf("you are number %d of %d", i+1, len(q.Jobs))
		}
	}
	return fmt.Sprintf("%d, none of them yours", len(q.Jobs))
}

func countOf(q Queue, user string) (n int) {
	for _, j := range q.Jobs {
		if j.User == user {
			n++
		}
	}
	return n
}

func (c *canvas) nameWidth() int {
	if c.Width == 0 {
		return 32
	}
	return min(32, max(10, c.Width-52))
}

func units(j *Job) string {
	if j.Estimated {
		return "~" + j.Units.String()
	}
	return j.Units.String()
}

func cut(s string, n int) string {
	if s = Clean(s); len(s) > n {
		return s[:n-1] + ">"
	}
	return s
}

func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// Frame fits lines into a terminal that is in raw mode, from line top on as far as
// there are any. It returns the first line shown.
func Frame(lines []string, top, height int) (string, int) {
	room := max(1, height-1)
	top = max(0, min(top, len(lines)-room))
	end := min(len(lines), top+room)
	var b strings.Builder
	b.WriteString("\x1b[H")
	for _, l := range lines[top:end] {
		b.WriteString(l + "\x1b[K\r\n")
	}
	foot := "q quits"
	if len(lines) > room {
		foot = fmt.Sprintf("%d-%d of %d  j k scroll  q quits", top+1, end, len(lines))
	}
	if height > 1 {
		b.WriteString(foot + "\x1b[K")
	}
	b.WriteString("\x1b[J")
	return b.String(), top
}

// Key maps a key press to a letter: j and k, d and u for pages, g and G for the ends.
func Key(input []byte) byte {
	switch string(input) {
	case "":
		return 0
	case "\x1b[A", "\x1bOA":
		return 'k'
	case "\x1b[B", "\x1bOB":
		return 'j'
	case "\x1b[5~":
		return 'u'
	case "\x1b[6~", " ":
		return 'd'
	case "\x1b[H", "\x1bOH", "\x1b[1~":
		return 'g'
	case "\x1b[F", "\x1bOF", "\x1b[4~":
		return 'G'
	}
	return input[0]
}

// Scroll is the first line to show after a key. Frame keeps it within bounds.
func Scroll(key byte, top, height int) int {
	switch key {
	case 'j':
		return top + 1
	case 'k':
		return top - 1
	case 'd':
		return top + max(1, height-2)
	case 'u':
		return top - max(1, height-2)
	case 'g':
		return 0
	case 'G':
		return 1 << 20
	}
	return top
}
