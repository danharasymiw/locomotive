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

// languageSeries plots the top extensions by final size, with the rest folded into "other".
func languageSeries(cs []commit, top int) []series {
	if len(cs) == 0 {
		return nil
	}
	last := cs[len(cs)-1].ByExt
	var exts []string
	for e, n := range last {
		if n > 0 {
			exts = append(exts, e)
		}
	}
	sort.Slice(exts, func(i, j int) bool {
		if last[exts[i]] != last[exts[j]] {
			return last[exts[i]] > last[exts[j]]
		}
		return exts[i] < exts[j]
	})
	keep := exts
	if len(keep) > top {
		keep = keep[:top]
	}
	isKept := map[string]bool{}
	ss := make([]series, 0, len(keep)+1)
	for i, e := range keep {
		isKept[e] = true
		ss = append(ss, series{name: e, color: palette[i%len(palette)]})
	}
	other := series{name: "other", color: palette[len(palette)-1]}
	for _, c := range cs {
		rest := 0
		for e, n := range c.ByExt {
			if !isKept[e] {
				rest += n
			}
		}
		for i, e := range keep {
			ss[i].pts = append(ss[i].pts, point{c.T, float64(c.ByExt[e])})
		}
		other.pts = append(other.pts, point{c.T, float64(rest)})
	}
	if len(exts) > top {
		ss = append(ss, other)
	}
	return ss
}

func churnSeries(cs []commit) []series {
	add := series{name: "added", color: lipgloss.Color("42")}
	del := series{name: "deleted", color: lipgloss.Color("203")}
	for _, c := range cs {
		add.pts = append(add.pts, point{c.T, float64(c.Added)})
		del.pts = append(del.pts, point{c.T, float64(c.Deleted)})
	}
	return []series{add, del}
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
