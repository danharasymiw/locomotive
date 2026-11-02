package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/NimbleMarkets/ntcharts/linechart/timeserieslinechart"
	"github.com/charmbracelet/lipgloss"
)

var (
	lineColor = lipgloss.Color("39")
	axisStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	palette   = []lipgloss.Color{"39", "208", "42", "170", "220", "81", "245"}
)

type point struct {
	t time.Time
	v float64
}

type series struct {
	name  string
	color lipgloss.Color
	pts   []point
}

// geom lets mouse positions be mapped back to times on the last rendered chart.
type geom struct {
	originX, graphW int
	t0, t1          time.Time
}

func (g geom) timeAt(x int) (time.Time, bool) {
	if g.graphW < 2 {
		return time.Time{}, false
	}
	f := float64(x-g.originX-1) / float64(g.graphW-1)
	if f < 0 || f > 1 {
		return time.Time{}, false
	}
	return g.t0.Add(time.Duration(f * float64(g.t1.Sub(g.t0)))), true
}

// render draws all series on one time axis. cursor, when non-nil, highlights that column.
func render(w, h int, ss []series, live bool, cursor *time.Time) (string, geom) {
	ss = thin(ss, w*4)
	if len(ss) == 0 || len(ss[0].pts) < 2 {
		return "not enough data yet…", geom{}
	}
	lo, hi := ss[0].pts[0].v, ss[0].pts[0].v
	t0, t1 := ss[0].pts[0].t, ss[0].pts[0].t
	for _, s := range ss {
		for _, p := range s.pts {
			lo, hi = min(lo, p.v), max(hi, p.v)
			if p.t.Before(t0) {
				t0 = p.t
			}
			if p.t.After(t1) {
				t1 = p.t
			}
		}
	}
	pad := (hi - lo) * 0.05
	if pad == 0 {
		pad = 1
	}
	lo, hi = lo-pad, hi+pad
	if !live && lo < 0 {
		lo = 0
	}
	if !t1.After(t0) {
		t1 = t0.Add(time.Minute)
	}
	xfmt := timeserieslinechart.DateTimeLabelFormatter()
	if live {
		xfmt = timeserieslinechart.HourTimeLabelFormatter()
	}
	c := timeserieslinechart.New(w, h,
		timeserieslinechart.WithTimeRange(t0, t1),
		timeserieslinechart.WithYRange(lo, hi),
		timeserieslinechart.WithXLabelFormatter(xfmt),
		timeserieslinechart.WithYLabelFormatter(func(_ int, v float64) string { return fmt.Sprintf("%.0f", v) }),
		timeserieslinechart.WithAxesStyles(axisStyle, axisStyle),
	)
	names := make([]string, len(ss))
	for i, s := range ss {
		names[i] = s.name
		c.SetDataSetStyle(s.name, lipgloss.NewStyle().Foreground(s.color))
		for _, p := range s.pts {
			c.PushDataSet(s.name, timeserieslinechart.TimePoint{Time: p.t, Value: p.v})
		}
	}
	c.DrawBrailleDataSets(names)
	if cursor != nil {
		// highlight needs the default data set to scale against
		c.PushDataSet(timeserieslinechart.DefaultDataSetName, timeserieslinechart.TimePoint{Time: t0, Value: lo})
		c.PushDataSet(timeserieslinechart.DefaultDataSetName, timeserieslinechart.TimePoint{Time: t1, Value: lo})
		c.SetColumnBackgroundStyle(*cursor, lipgloss.NewStyle().Background(lipgloss.Color("237")))
	}
	return c.View(), geom{originX: c.Origin().X, graphW: c.GraphWidth(), t0: t0, t1: t1}
}

func legend(ss []series) string {
	if len(ss) < 2 {
		return ""
	}
	var parts []string
	for _, s := range ss {
		parts = append(parts, lipgloss.NewStyle().Foreground(s.color).Render("━ "+s.name))
	}
	return strings.Join(parts, "  ")
}

// --- series builders ---

func totalSeries(cs []commit) []series {
	s := series{name: "total", color: lineColor}
	for _, c := range cs {
		s.pts = append(s.pts, point{c.T, float64(c.Total)})
	}
	return []series{s}
}

func authorKey(c commit) string { return c.Author }

func aiKey(c commit) string {
	switch {
	case c.AI:
		return "ai-assisted"
	case c.Bot:
		return "bot"
	}
	return "human"
}

// groupSeries plots each group's running net lines (added minus deleted), ranking groups
// by total lines added and folding everything past top into "other".
func groupSeries(cs []commit, key func(commit) string, top int) []series {
	added := map[string]int{}
	for _, c := range cs {
		added[key(c)] += c.Added
	}
	names := make([]string, 0, len(added))
	for n := range added {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if added[names[i]] != added[names[j]] {
			return added[names[i]] > added[names[j]]
		}
		return names[i] < names[j]
	})
	keep := names[:min(top, len(names))]
	isKept := map[string]bool{}
	ss := make([]series, len(keep))
	for i, n := range keep {
		isKept[n] = true
		ss[i] = series{name: n, color: palette[i%len(palette)]}
	}
	other := series{name: "other", color: palette[len(palette)-1]}
	run := map[string]int{}
	rest := 0
	for _, c := range cs {
		k, d := key(c), c.Added-c.Deleted
		run[k] += d
		if !isKept[k] {
			rest += d
		}
		for i, n := range keep {
			ss[i].pts = append(ss[i].pts, point{c.T, float64(run[n])})
		}
		other.pts = append(other.pts, point{c.T, float64(rest)})
	}
	if len(names) > top {
		ss = append(ss, other)
	}
	return ss
}

// groupTotals returns each group's running net lines through commit index upto.
func groupTotals(cs []commit, upto int, key func(commit) string) map[string]int {
	run := map[string]int{}
	for _, c := range cs[:min(upto+1, len(cs))] {
		run[key(c)] += c.Added - c.Deleted
	}
	return run
}

func commas(n int) string {
	s := fmt.Sprint(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// whoLines returns each person's lines added in AI-assisted commits through commit index upto.
// Bot accounts are left out, since they are not people choosing to use AI.
func whoLines(cs []commit, upto int) map[string]int {
	out := map[string]int{}
	for _, c := range cs[:min(upto+1, len(cs))] {
		if !c.Bot && !c.Agent {
			out[c.Author] += map[bool]int{true: c.Added}[c.AI]
		}
	}
	return out
}

// whoSeries plots, per person, the running lines added in AI-assisted commits, ranking people
// by their AI lines.
func whoSeries(cs []commit, top int) []series {
	total := whoLines(cs, len(cs)-1)
	names := make([]string, 0, len(total))
	for n := range total {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if total[names[i]] != total[names[j]] {
			return total[names[i]] > total[names[j]]
		}
		return names[i] < names[j]
	})
	names = names[:min(top, len(names))]
	idx := map[string]int{}
	ss := make([]series, len(names))
	for i, n := range names {
		idx[n] = i
		ss[i] = series{name: n, color: palette[i%len(palette)]}
	}
	run := map[string]int{}
	for _, c := range cs {
		i, ok := idx[c.Author]
		if !ok || c.Bot || c.Agent {
			continue
		}
		if c.AI {
			run[c.Author] += c.Added
		}
		ss[i].pts = append(ss[i].pts, point{c.T, float64(run[c.Author])})
	}
	return ss
}

// thin keeps long series drawable: past limit points it keeps each bucket's lowest and highest
// point (in time order), so spikes survive while the chart does far less work.
func thin(ss []series, limit int) []series {
	out := make([]series, len(ss))
	for i, s := range ss {
		out[i] = s
		n := len(s.pts)
		if n <= limit || limit < 4 {
			continue
		}
		buckets := limit / 2
		pts := make([]point, 0, limit)
		for b := 0; b < buckets; b++ {
			seg := s.pts[b*n/buckets : (b+1)*n/buckets]
			if len(seg) == 0 {
				continue
			}
			lo, hi := 0, 0
			for j, p := range seg {
				if p.v < seg[lo].v {
					lo = j
				}
				if p.v > seg[hi].v {
					hi = j
				}
			}
			if lo > hi {
				lo, hi = hi, lo
			}
			pts = append(pts, seg[lo])
			if hi != lo {
				pts = append(pts, seg[hi])
			}
		}
		if last := s.pts[n-1]; pts[len(pts)-1] != last {
			pts = append(pts, last)
		}
		out[i].pts = pts
	}
	return out
}

// aiShare is the percentage of lines added in AI-assisted commits, over the given commits. Bot
// accounts are left out, as in the who-uses-ai view. ok is false when nothing was added.
func aiShare(cs []commit) (pct int, ok bool) {
	var added, ai int
	for _, c := range cs {
		if c.Bot {
			continue
		}
		added += c.Added
		if c.AI {
			ai += c.Added
		}
	}
	if added == 0 {
		return 0, false
	}
	return ai * 100 / added, true
}
