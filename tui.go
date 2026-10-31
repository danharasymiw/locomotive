package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	pollEvery = time.Second
	maxLive   = 600 // samples kept for the live chart
	chromeH   = 3   // header, tabs, blank line above the chart
)

type mode int

const (
	modeTotal mode = iota
	modeLang
	modeChurn
	modeAuthors
	modeAI
	numModes
)

func (m mode) String() string {
	return [...]string{"total", "languages", "churn", "authors", "human vs ai"}[m]
}

type view int

const (
	viewHist view = iota
	viewLive
	viewWT
	numViews
)

type (
	wtMsg struct {
		stats []wtStat
		err   error
	}
	histMsg struct {
		h   history
		err error
	}
	liveMsg struct {
		s   liveStat
		err error
	}
	tickMsg struct{}
)

type model struct {
	o          opts
	hist       history
	live       []point
	stat       liveStat
	head       string
	cur        int // cursor index into hist.Commits
	followLast bool
	view       view
	wt         []wtStat
	wtPts      map[string][]point // net uncommitted lines per worktree over time
	mode       mode
	w, h       int
	err        error
}

func newModel(o opts) model { return model{o: o, followLast: true, wtPts: map[string][]point{}} }

func (m model) Init() tea.Cmd { return tea.Batch(m.loadHist(false), m.poll(), m.pollWT(), tick()) }

func (m model) pollWT() tea.Cmd {
	o := m.o
	return func() tea.Msg {
		s, err := pollWorktrees(o)
		return wtMsg{s, err}
	}
}

func tick() tea.Cmd { return tea.Tick(pollEvery, func(time.Time) tea.Msg { return tickMsg{} }) }

func (m model) loadHist(force bool) tea.Cmd {
	o, prev := m.o, m.hist
	return func() tea.Msg {
		h, err := loadCached(o, prev, force)
		return histMsg{h, err}
	}
}

func (m model) poll() tea.Cmd {
	o := m.o
	return func() tea.Msg {
		s, err := loadLive(o)
		return liveMsg{s, err}
	}
}

func (m model) base() float64 {
	if n := len(m.hist.Commits); n > 0 {
		return float64(m.hist.Commits[n-1].Total)
	}
	return 0
}

func (m *model) moveCursor(d int) {
	n := len(m.hist.Commits)
	if n == 0 {
		return
	}
	m.cur = max(0, min(n-1, m.cur+d))
	m.followLast = m.cur == n-1
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "tab":
			m.view = (m.view + 1) % numViews
		case "m":
			m.mode = (m.mode + 1) % numModes
		case "left", "h":
			m.moveCursor(-1)
		case "right", "l":
			m.moveCursor(1)
		case "shift+left", "H":
			m.moveCursor(-10)
		case "shift+right", "L":
			m.moveCursor(10)
		case "home", "g":
			m.cur, m.followLast = 0, false
		case "end", "G":
			m.moveCursor(len(m.hist.Commits))
		case "r":
			m.live = nil
			return m, m.loadHist(true)
		}
	case tea.MouseMsg:
		motion := msg.Action == tea.MouseActionMotion
		click := msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft
		if (motion || click) && m.view == viewHist && msg.Y >= chromeH && len(m.hist.Commits) > 1 {
			_, g := m.draw()
			if t, ok := g.timeAt(msg.X); ok {
				cs := m.hist.Commits
				i := sort.Search(len(cs), func(i int) bool { return !cs[i].T.Before(t) })
				if i > 0 && (i == len(cs) || t.Sub(cs[i-1].T) < cs[i].T.Sub(t)) {
					i--
				}
				m.cur = min(i, len(cs)-1)
				m.followLast = m.cur == len(cs)-1
			}
		}
	case histMsg:
		m.err = msg.err
		if msg.err == nil {
			m.hist = msg.h
			if m.followLast || m.cur >= len(m.hist.Commits) {
				m.cur = max(0, len(m.hist.Commits)-1)
			}
		}
	case tickMsg:
		return m, tea.Batch(m.poll(), m.pollWT(), tick())
	case wtMsg:
		if msg.err != nil {
			break
		}
		m.wt = msg.stats
		now := time.Now()
		for _, s := range msg.stats {
			pts := append(m.wtPts[s.wt.name], point{now, float64(s.delta)})
			m.wtPts[s.wt.name] = pts[max(0, len(pts)-maxLive):]
		}
	case liveMsg:
		if msg.err != nil {
			m.err = msg.err
			break
		}
		m.err = nil
		m.stat = msg.s
		if m.head != "" && msg.s.head != m.head {
			m.head = msg.s.head // a commit landed: pick up just the new ones
			return m, m.loadHist(false)
		}
		m.head = msg.s.head
		if len(m.hist.Commits) > 0 {
			m.live = append(m.live, point{time.Now(), m.base() + float64(msg.s.delta())})
			if len(m.live) > maxLive {
				m.live = m.live[len(m.live)-maxLive:]
			}
		}
	}
	return m, nil
}

func (m model) chartH() int { return max(m.h-chromeH-4, 5) }

func (m model) seriesFor() []series {
	cs := m.hist.Commits
	switch m.mode {
	case modeLang:
		return append([]series{whiteTotal(cs)}, languageSeries(cs, 5)...)
	case modeChurn:
		return churnSeries(cs)
	case modeAuthors:
		return append([]series{whiteTotal(cs)}, groupSeries(cs, authorKey, 5)...)
	case modeAI:
		ss := groupSeries(cs, aiKey, 2)
		for i := range ss {
			ss[i].color = map[string]lipgloss.Color{"human": "39", "ai-assisted": "208"}[ss[i].name]
		}
		return append([]series{whiteTotal(cs)}, ss...)
	}
	return totalSeries(cs)
}

// draw renders the active chart and returns its geometry.
func (m model) draw() (string, geom) {
	w := max(m.w-2, 10)
	switch m.view {
	case viewLive:
		return render(w, m.chartH(), []series{{"live", lineColor, m.live}}, true, nil)
	case viewWT:
		var ss []series
		for i, s := range m.wt {
			ss = append(ss, series{s.wt.name, palette[i%len(palette)], m.wtPts[s.wt.name]})
		}
		body, g := render(w, m.chartH(), ss, true, nil)
		return body + "\n" + m.wtLegend(ss), g
	}
	var cursor *time.Time
	if m.cur < len(m.hist.Commits) {
		t := m.hist.Commits[m.cur].T
		cursor = &t
	}
	ss := m.seriesFor()
	body, g := render(w, m.chartH(), ss, false, cursor)
	if lg := legend(ss); lg != "" {
		body += "\n" + lg
	}
	return body, g
}

func (m model) header() string {
	bold := lipgloss.NewStyle().Bold(true)
	green := lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	red := lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dim := axisStyle

	total := int(m.base()) + m.stat.delta()
	s := bold.Render(commas(total)+" lines") + dim.Render(fmt.Sprintf("  %d commits  ", len(m.hist.Commits)))
	s += green.Render(fmt.Sprintf("+%s", commas(m.stat.added+m.stat.untracked))) + " " + red.Render(fmt.Sprintf("-%s", commas(m.stat.deleted)))
	s += dim.Render(" uncommitted")
	if len(m.live) > 0 {
		d := int(m.live[len(m.live)-1].v - m.live[0].v)
		s += dim.Render("  session ") + fmt.Sprintf("%+d", d)
	}
	return s
}

// wtLegend lists one row per worktree: name, branch, status and net uncommitted lines.
func (m model) wtLegend(ss []series) string {
	rows := make([]string, len(m.wt))
	for i, s := range m.wt {
		name := lipgloss.NewStyle().Foreground(ss[i].color).Render("━ " + s.wt.name)
		status := ""
		if st := s.status(); st != "" {
			status = " " + lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("["+st+"]")
		}
		rows[i] = fmt.Sprintf("%s%s %s  %+d (+%d −%d)", name, status, axisStyle.Render(s.wt.branch), s.delta, s.added, s.deleted)
	}
	return lipgloss.NewStyle().MaxWidth(m.w).Render(strings.Join(rows, "\n"))
}

func (m model) info() string {
	if m.view != viewHist || m.cur >= len(m.hist.Commits) {
		return ""
	}
	c := m.hist.Commits[m.cur]
	hash := c.Hash
	if len(hash) > 7 {
		hash = hash[:7]
	}
	s := fmt.Sprintf("%s  %s  %s  %s — %s lines (+%d −%d)",
		lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(hash),
		c.T.Format("2006-01-02"), c.Author, c.Subject, commas(c.Total), c.Added, c.Deleted)
	if m.mode == modeLang {
		var exts []string
		for e, n := range c.ByExt {
			if n > 0 {
				exts = append(exts, e)
			}
		}
		sort.Slice(exts, func(i, j int) bool { return c.ByExt[exts[i]] > c.ByExt[exts[j]] })
		var parts []string
		for _, e := range exts[:min(len(exts), 6)] {
			parts = append(parts, fmt.Sprintf("%s %s", e, commas(c.ByExt[e])))
		}
		s += "\n" + axisStyle.Render(strings.Join(parts, " · "))
	}
	var key func(commit) string
	switch m.mode {
	case modeAuthors:
		key = authorKey
	case modeAI:
		key = aiKey
	}
	if key != nil {
		run := groupTotals(m.hist.Commits, m.cur, key)
		var names []string
		for n := range run {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return run[names[i]] > run[names[j]] })
		var parts []string
		for _, n := range names[:min(len(names), 6)] {
			part := fmt.Sprintf("%s %s", n, commas(run[n]))
			if m.mode == modeAI && c.Total > 0 {
				part += fmt.Sprintf(" (%d%%)", run[n]*100/c.Total)
			}
			parts = append(parts, part)
		}
		s += "\n" + axisStyle.Render(strings.Join(parts, " · "))
	}
	return lipgloss.NewStyle().MaxWidth(m.w).Render(s)
}

func (m model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	on := lipgloss.NewStyle().Bold(true).Foreground(lineColor).Underline(true)
	tab := func(name string, active bool) string {
		if active {
			return on.Render(name)
		}
		return axisStyle.Render(name)
	}
	histLabel := "history"
	if m.view == viewHist {
		histLabel += " (" + m.mode.String() + ")" // mode only applies to this view
	}
	tabs := tab(histLabel, m.view == viewHist) + "  " +
		tab("live", m.view == viewLive) + "  " + tab("worktrees", m.view == viewWT)

	var body string
	switch {
	case m.err != nil:
		body = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render(m.err.Error())
	case len(m.hist.Commits) == 0 && m.view == viewHist:
		body = "loading history…"
	default:
		body, _ = m.draw()
	}
	help := axisStyle.Render("tab view · m mode · ←/→ cursor (shift ×10) · r reload · q quit")
	return strings.Join([]string{m.header(), tabs, "", body, m.info(), help}, "\n")
}
