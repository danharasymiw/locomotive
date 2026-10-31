// LoComotive plots a repo's lines of code over time: per commit, plus a live view
// of uncommitted changes in the working tree.
package main

import (
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	var (
		o       opts
		once    = flag.Bool("once", false, "print the chart once and exit")
		modeArg = flag.String("mode", "total", "with -once: total, languages, churn, authors, ai, who or worktrees")
		asJSON  = flag.Bool("json", false, "print per-commit data as JSON and exit")
		asCSV   = flag.Bool("csv", false, "print per-commit data as CSV and exit")
		exts    = flag.String("ext", "", "only count these extensions, comma separated (e.g. go,ts)")
		excl    = flag.String("exclude", "", "extra globs to ignore, comma separated (e.g. '**/*.pb.go')")
	)
	flag.StringVar(&o.branch, "branch", "", "branch or ref to walk (default HEAD)")
	flag.StringVar(&o.author, "author", "", "only count commits by this author (git --author pattern)")
	sinceArg := flag.String("since", "", "history window: 1m, 3m, 6m, 1y, all, or a date like 2025-01-01 (default 3m)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: locomotive [flags] [repo-dir]")
		flag.PrintDefaults()
	}
	flag.Parse()

	o.dir = "."
	if flag.NArg() > 0 {
		o.dir = flag.Arg(0)
	}
	o.name = repoName(o.dir)
	var label string
	var err error
	if o.since, label, err = resolveSince(*sinceArg); err != nil {
		fatal(err.Error())
	}
	o.exts = map[string]bool{}
	for _, e := range splitList(*exts) {
		o.exts[strings.ToLower(strings.TrimPrefix(e, "."))] = true
	}
	o.exclude = splitList(*excl)

	if _, err := git(o.dir, "rev-parse", "--git-dir"); err != nil {
		fatal("not a git repository: " + o.dir)
	}

	switch {
	case *asJSON || *asCSV:
		h, _, err := loadCached(o, ledger{}, false)
		if err != nil {
			fatal(err.Error())
		}
		if *asJSON {
			writeJSON(h.Commits)
		} else {
			writeCSV(h.Commits)
		}
	case *once && *modeArg == "worktrees":
		stats, err := pollWorktrees(o)
		if err != nil {
			fatal(err.Error())
		}
		m := newModel(o, label)
		m.w, m.wt = 100, stats
		ss := make([]series, len(stats))
		for i := range stats {
			ss[i].color = palette[i%len(palette)]
		}
		fmt.Println(m.wtLegend(ss))
	case *once:
		h, _, err := loadCached(o, ledger{}, false)
		if err != nil {
			fatal(err.Error())
		}
		m := newModel(o, label)
		m.hist, m.loaded, m.w, m.h = h, true, 100, 28
		m.cur = max(0, len(h.Commits)-1)
		m.stat, _ = loadLive(o)
		for i := range numModes {
			if s := i.String(); s == *modeArg || (i == modeAI && *modeArg == "ai") || (i == modeWho && *modeArg == "who") {
				m.mode = i
			}
		}
		body, _ := m.draw()
		fmt.Println(withBrand(m.header(), m.w))
		fmt.Println(body)
		fmt.Println(m.info())
	default:
		if _, err := tea.NewProgram(newModel(o, label), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run(); err != nil {
			fatal(err.Error())
		}
	}
}

func splitList(s string) (out []string) {
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "derailed: "+msg)
	os.Exit(1)
}

func writeJSON(cs []commit) {
	type row struct {
		Time    string         `json:"time"`
		Hash    string         `json:"hash"`
		Author  string         `json:"author"`
		Subject string         `json:"subject"`
		AI      bool           `json:"ai"`
		Total   int            `json:"total"`
		Added   int            `json:"added"`
		Deleted int            `json:"deleted"`
		ByExt   map[string]int `json:"by_ext"`
	}
	rows := make([]row, len(cs))
	for i, c := range cs {
		rows[i] = row{c.T.Format("2006-01-02T15:04:05Z07:00"), c.Hash, c.Author, c.Subject, c.AI, c.Total, c.Added, c.Deleted, c.ByExt}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(rows)
}

func writeCSV(cs []commit) {
	w := csv.NewWriter(os.Stdout)
	w.Write([]string{"time", "hash", "author", "subject", "ai", "total", "added", "deleted"})
	for _, c := range cs {
		w.Write([]string{c.T.Format("2006-01-02T15:04:05Z07:00"), c.Hash, c.Author, c.Subject,
			strconv.FormatBool(c.AI), strconv.Itoa(c.Total), strconv.Itoa(c.Added), strconv.Itoa(c.Deleted)})
	}
	w.Flush()
}
