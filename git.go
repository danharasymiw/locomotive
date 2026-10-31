package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxFileSize = 1 << 20

// Noise that would swamp the real code size.
var excludes = exclude(
	// lockfiles (*.lock covers Cargo, yarn, Gemfile, poetry, Pipfile, composer, pub, mix, Podfile)
	"**/*.lock", "**/go.sum", "**/go.work.sum", "**/package-lock.json", "**/npm-shrinkwrap.json",
	"**/pnpm-lock.yaml", "**/packages.lock.json", "**/gradle.lockfile",
	// vendored and installed dependencies
	"**/vendor/**", "**/node_modules/**", "**/third_party/**", "**/Pods/**",
	"**/bower_components/**", "**/.venv/**", "**/venv/**", "**/site-packages/**",
	// build and tool output
	"**/dist/**", "**/.next/**", "**/.nuxt/**", "**/coverage/**", "**/__pycache__/**",
	"**/.idea/**", "**/.vscode/**",
	// minified and generated code
	"**/*.min.js", "**/*.min.css", "**/*.map", "**/*.pb.go", "**/*.pb.gw.go", "**/*.pb.cc",
	"**/*.pb.h", "**/*_pb2.py", "**/*_pb2_grpc.py", "**/*.g.dart", "**/*.freezed.dart",
	"**/*.designer.cs", "**/*.generated.*", "**/zz_generated*",
)

func exclude(globs ...string) []string {
	ps := make([]string, len(globs))
	for i, g := range globs {
		ps[i] = ":(exclude,glob)" + g
	}
	return ps
}

type opts struct {
	dir, name      string // name is display only, not part of the cache key
	branch, author string
	since          time.Time       // start of the history window; zero = all
	exts           map[string]bool // lowercase, no leading dot; empty = all
	exclude        []string        // extra globs
}

// repoName names the repository, not the checkout: a linked worktree reports its main repo.
func repoName(dir string) string {
	if out, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		if common := strings.TrimSpace(string(out)); filepath.Base(common) == ".git" {
			return filepath.Base(filepath.Dir(common))
		}
	}
	abs, _ := filepath.Abs(dir)
	return filepath.Base(abs)
}

var periods = []struct {
	label  string
	months int // 0 = all history
}{{"1m", 1}, {"3m", 3}, {"6m", 6}, {"1y", 12}, {"all", 0}}

// resolveSince turns a window label or a YYYY-MM-DD date into a start time (zero = all history).
func resolveSince(s string) (time.Time, string, error) {
	if s == "" {
		s = "3m"
	}
	for _, p := range periods {
		if p.label == s {
			if p.months == 0 {
				return time.Time{}, s, nil
			}
			return time.Now().AddDate(0, -p.months, 0), s, nil
		}
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, s, fmt.Errorf("bad --since %q: use 1m, 3m, 6m, 1y, all or YYYY-MM-DD", s)
	}
	return t, s, nil
}

// loadBaseline counts the lines in every tracked text file at rev, in total and per extension.
func loadBaseline(o opts, rev string) (int, map[string]int, error) {
	args := append([]string{"grep", "-I", "-c", "", rev}, o.pathspec()...)
	out, err := git(o.dir, args...)
	if err != nil {
		return 0, nil, err
	}
	total, by := 0, map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimPrefix(line, rev+":")
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		n, err := strconv.Atoi(line[i+1:])
		if err != nil || !o.wants(line[:i]) {
			continue
		}
		total += n
		by[extOf(line[:i])] += n
	}
	return total, by, nil
}

func (o opts) pathspec() []string {
	return append(append([]string{"--", "."}, excludes...), exclude(o.exclude...)...)
}

func (o opts) rev() string {
	if o.branch != "" {
		return o.branch
	}
	return "HEAD"
}

func (o opts) wants(path string) bool { return len(o.exts) == 0 || o.exts[extOf(path)] }

func (o opts) key() string {
	abs, _ := filepath.Abs(o.dir)
	var exts []string
	for e := range o.exts {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	parts := []string{abs, o.rev(), o.author, strings.Join(exts, ","), strings.Join(o.exclude, ","), "v4"}
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

func git(dir string, args ...string) ([]byte, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

var renameBrace = regexp.MustCompile(`\{[^}]* => ([^}]*)\}`)

// extOf returns the lowercase extension (no dot), or the basename for extensionless files.
// It understands numstat rename syntax ("a => b", "dir/{a => b}/f").
func extOf(path string) string {
	if i := strings.LastIndex(path, " => "); i >= 0 {
		if renameBrace.MatchString(path) {
			path = renameBrace.ReplaceAllString(path, "$1")
		} else {
			path = path[i+4:]
		}
	}
	base := strings.ToLower(filepath.Base(path))
	if e := filepath.Ext(base); e != "" && e != base {
		return e[1:]
	}
	return strings.TrimPrefix(base, ".")
}

func parseNumstat(line string) (added, deleted int, path string, ok bool) {
	f := strings.SplitN(line, "\t", 3)
	if len(f) != 3 {
		return
	}
	a, err1 := strconv.Atoi(f[0])
	d, err2 := strconv.Atoi(f[1]) // binary files report "-"
	if err1 != nil || err2 != nil {
		return
	}
	return a, d, f[2], true
}

var aiName = regexp.MustCompile(`(?i)claude|anthropic|copilot|cursor|aider|codex|gemini|openai|chatgpt|devin|windsurf`)

// aiTrailer reports whether any Co-Authored-By value names a known coding agent.
func aiTrailer(trailers string) bool { return aiName.MatchString(trailers) }

type commit struct {
	Hash, Author, Subject string
	AI                    bool // a Co-Authored-By trailer names a coding agent
	T                     time.Time
	Total, Added, Deleted int
	ByExt                 map[string]int
}

// history is what the UI plots: the commits inside the chosen window, with running totals.
type history struct {
	Head    string
	Commits []commit
}

// delta is one commit's effect on line counts, independent of any window.
type delta struct {
	Hash, Author, Subject string
	AI                    bool
	T                     time.Time
	Added, Deleted        int
	ByExt                 map[string]int // net lines per extension
}

// ledger is the cached commit log. It holds every commit reachable from Head with a commit time
// at or after From (or all of history when All), so any window inside that range needs no git walk.
type ledger struct {
	Head   string
	From   time.Time
	All    bool
	Deltas []delta // oldest first
}

func gitDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// fetchDeltas reads commits oldest first. args select them (date limits and a rev or range).
func fetchDeltas(o opts, args ...string) ([]delta, error) {
	full := []string{"log", "--reverse", "--no-merges", "-M", "--numstat",
		"--format=@%H%x09%ct%x09%aN%x09%(trailers:key=Co-Authored-By,valueonly,unfold,separator=%x1F)%x09%s"}
	if o.author != "" {
		full = append(full, "--author="+o.author)
	}
	out, err := git(o.dir, append(append(full, args...), o.pathspec()...)...)
	if err != nil {
		return nil, err
	}
	var (
		ds  []delta
		cur *delta
	)
	flush := func() {
		if cur != nil {
			ds = append(ds, *cur)
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "@"):
			flush()
			f := append(strings.SplitN(line[1:], "\t", 5), "", "", "", "", "")
			secs, _ := strconv.ParseInt(f[1], 10, 64)
			cur = &delta{Hash: f[0], T: time.Unix(secs, 0), Author: f[2], AI: aiTrailer(f[3]), Subject: f[4], ByExt: map[string]int{}}
		case line != "" && cur != nil:
			if a, d, p, ok := parseNumstat(line); ok && o.wants(p) {
				cur.Added += a
				cur.Deleted += d
				cur.ByExt[extOf(p)] += a - d
			}
		}
	}
	flush()
	return ds, sc.Err()
}

// syncLedger brings led up to date: new commits on top, and older ones if since reaches
// back further than what is already covered. Only the missing range is read from git.
func syncLedger(o opts, led ledger, since time.Time) (ledger, error) {
	out, err := git(o.dir, "rev-parse", o.rev())
	if err != nil {
		return ledger{}, err
	}
	head := strings.TrimSpace(string(out))
	if led.Head != "" && led.Head != head {
		if _, err := git(o.dir, "merge-base", "--is-ancestor", led.Head, head); err != nil {
			led = ledger{} // history was rewritten
		} else {
			newer, err := fetchDeltas(o, led.Head+".."+head)
			if err != nil {
				return ledger{}, err
			}
			led.Deltas, led.Head = append(led.Deltas, newer...), head
		}
	}
	if led.Head == "" {
		args := []string{head}
		if !since.IsZero() {
			args = []string{"--since=" + gitDate(since), head}
		}
		ds, err := fetchDeltas(o, args...)
		return ledger{Head: head, From: since, All: since.IsZero(), Deltas: ds}, err
	}
	if !led.All && (since.IsZero() || since.Before(led.From)) {
		// the 24h overlap guards the boundary; duplicates are dropped by hash
		args := []string{"--until=" + gitDate(led.From.Add(24*time.Hour)), head}
		if !since.IsZero() {
			args = append([]string{"--since=" + gitDate(since)}, args...)
		}
		older, err := fetchDeltas(o, args...)
		if err != nil {
			return ledger{}, err
		}
		have := make(map[string]bool, len(led.Deltas))
		for _, d := range led.Deltas {
			have[d.Hash] = true
		}
		merged := make([]delta, 0, len(older)+len(led.Deltas))
		for _, d := range older {
			if !have[d.Hash] {
				merged = append(merged, d)
			}
		}
		led.Deltas, led.From, led.All = append(merged, led.Deltas...), since, since.IsZero()
	}
	return led, nil
}

// derive builds the plotted history for a window: running totals over the whole ledger, cut to
// the commits at or after since, then shifted so the numbers mean something.
func derive(o opts, led ledger, since time.Time) history {
	var (
		win     []commit
		total   int
		by      = map[string]int{}
		base0   int
		base0By map[string]int
		started bool
		prevT   time.Time
	)
	for _, d := range led.Deltas {
		t := d.T
		if t.Before(prevT) {
			t = prevT // rebased commits can have out-of-order timestamps
		}
		prevT = t
		if !started && (since.IsZero() || !t.Before(since)) {
			started, base0, base0By = true, total, maps.Clone(by)
		}
		total += d.Added - d.Deleted
		for e, n := range d.ByExt {
			by[e] += n
		}
		if started {
			win = append(win, commit{Hash: d.Hash, Author: d.Author, Subject: d.Subject, AI: d.AI, T: t,
				Total: total, Added: d.Added, Deleted: d.Deleted, ByExt: maps.Clone(by)})
		}
	}
	if len(win) == 0 {
		return history{Head: led.Head}
	}

	off, offExt := 0, map[string]int{}
	relative := func() {
		off = -base0
		for e, n := range base0By {
			offExt[e] = -n
		}
	}
	switch {
	case o.author != "":
		relative() // one person's contribution: count from the start of the window
	case led.All:
		// the ledger starts at the first commit, so the sums are real file sizes already
	default:
		// the ledger starts mid-history: anchor its end to the actual line counts at HEAD
		abs, absBy, err := loadBaseline(o, led.Head)
		if err != nil {
			relative()
			break
		}
		off = max(0, abs-total)
		for e, n := range absBy {
			offExt[e] = max(0, n-by[e])
		}
	}
	for i := range win {
		win[i].Total += off
		for e, d := range offExt {
			win[i].ByExt[e] += d
		}
	}
	return history{Head: led.Head, Commits: win}
}

// --- cache ---

func cachePath(o opts) string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "slopmeter", o.key()+".gob")
}

func readCache(o opts) (l ledger) {
	f, err := os.Open(cachePath(o))
	if err != nil {
		return
	}
	defer f.Close()
	if gob.NewDecoder(f).Decode(&l) != nil {
		return ledger{}
	}
	return
}

func writeCache(o opts, l ledger) {
	p := cachePath(o)
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	err = gob.NewEncoder(f).Encode(l)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return
	}
	os.Rename(tmp, p)
}

// loadCached syncs the ledger (starting from led, or the on-disk cache if led is empty),
// saves it when it grew, and derives the history for o.since. force rebuilds from scratch.
func loadCached(o opts, led ledger, force bool) (history, ledger, error) {
	if force {
		led = ledger{}
	} else if led.Head == "" {
		led = readCache(o)
	}
	before := led
	led, err := syncLedger(o, led, o.since)
	if err != nil {
		return history{}, led, err
	}
	if led.Head != before.Head || !led.From.Equal(before.From) || led.All != before.All || len(led.Deltas) != len(before.Deltas) {
		writeCache(o, led)
	}
	return derive(o, led, o.since), led, nil
}

// --- live working tree ---

type liveStat struct {
	added, deleted, untracked int
	head                      string
}

func (s liveStat) delta() int { return s.added - s.deleted + s.untracked }

// loadLive measures uncommitted work: tracked diff against HEAD plus untracked files.
func loadLive(o opts) (liveStat, error) {
	out, err := git(o.dir, "rev-parse", o.rev())
	if err != nil {
		return liveStat{}, fmt.Errorf("no commits yet")
	}
	s := liveStat{head: strings.TrimSpace(string(out))}
	out, err = git(o.dir, append([]string{"diff", "HEAD", "--numstat"}, o.pathspec()...)...)
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if a, d, p, ok := parseNumstat(line); ok && o.wants(p) {
			s.added += a
			s.deleted += d
		}
	}
	out, err = git(o.dir, append([]string{"ls-files", "-z", "--others", "--exclude-standard"}, o.pathspec()...)...)
	if err != nil {
		return s, err
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" && o.wants(p) {
			s.untracked += countLines(filepath.Join(o.dir, p))
		}
	}
	return s, nil
}

func countLines(path string) int {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxFileSize {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}
