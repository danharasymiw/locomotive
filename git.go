package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxFileSize = 1 << 20

// Noise that would swamp the real code size.
var excludeGlobs = []string{
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
	// documents, media, fonts and archives: git can read these as text and miscount them
	"**/*.pdf", "**/*.doc", "**/*.docx", "**/*.xls", "**/*.xlsx", "**/*.ppt", "**/*.pptx",
	"**/*.png", "**/*.jpg", "**/*.jpeg", "**/*.gif", "**/*.ico", "**/*.webp", "**/*.svg",
	"**/*.woff", "**/*.woff2", "**/*.ttf", "**/*.otf", "**/*.eot",
	"**/*.zip", "**/*.gz", "**/*.tgz", "**/*.tar", "**/*.jar", "**/*.whl",
	"**/*.parquet", "**/*.pkl", "**/*.sqlite", "**/*.db",
}

var excludes = exclude(excludeGlobs...)

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

// attrExcludes drops paths the repo marks in .gitattributes as generated or vendored. Git reads
// the current attributes, so a path marked today is excluded from every commit of its history.
var attrExcludes = []string{
	":(exclude,attr:linguist-generated)", ":(exclude,attr:linguist-generated=true)",
	":(exclude,attr:linguist-vendored)", ":(exclude,attr:linguist-vendored=true)",
}

var (
	attrMu    sync.Mutex
	attrRules = map[string]bool{}
)

// usesAttrRules reports whether the repo declares any linguist attribute. The attr: pathspecs
// make every git log noticeably slower, so repos without such rules skip them.
func usesAttrRules(dir string) bool {
	attrMu.Lock()
	defer attrMu.Unlock()
	if v, ok := attrRules[dir]; ok {
		return v
	}
	_, err := git(dir, "grep", "--untracked", "-q", "-e", "linguist-", "--", ":(glob)**/.gitattributes")
	var ee *exec.ExitError
	found := !(errors.As(err, &ee) && ee.ExitCode() == 1) // exit 1 is "no match"; any other failure keeps the exclusions
	if !found {
		if p, err := git(dir, "rev-parse", "--git-path", "info/attributes"); err == nil {
			path := strings.TrimSpace(string(p))
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte("linguist-")) {
				found = true
			}
		}
	}
	attrRules[dir] = found
	return found
}

func (o opts) pathspec() []string {
	ps := append([]string{"--", "."}, excludes...)
	if usesAttrRules(o.dir) {
		ps = append(ps, attrExcludes...)
	}
	return append(ps, exclude(o.exclude...)...)
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
	parts := []string{abs, o.rev(), o.author, strings.Join(exts, ","), strings.Join(o.exclude, ","), "v6"}
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

// aiMessage matches the footers coding agents write into a commit message body.
var aiMessage = regexp.MustCompile(`(?i)generated with.*(claude|cursor|copilot|codex|gemini|windsurf)|made with cursor|🤖|cursoragent`)

// detectVersion invalidates cached ledgers whenever the AI heuristics change.
const detectVersion = 4

// botName matches automation accounts such as dependabot[bot] or renovate-bot.
var botName = regexp.MustCompile(`(?i)\[bot\]|[-_ ]bot$|^bot[-_ ]`)

// aiIdentity reports whether a name, email or trailer value names a known coding agent.
func aiIdentity(fields ...string) bool {
	for _, f := range fields {
		if aiName.MatchString(f) {
			return true
		}
	}
	return false
}

type commit struct {
	Hash, Author, Subject string
	AI                    bool // a trailer, footer or identity names a coding agent
	Bot                   bool // authored by an automation account
	Agent                 bool // authored by a coding agent with no human co-author to credit
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
	AI, Bot, Agent        bool
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
	Detect int     // detectVersion the AI flags were computed with
}

func gitDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

const (
	recStart = "\x01" // starts a commit record in the log output
	recEnd   = "\x02" // ends its header; numstat lines follow
	fieldSep = "\x1e"
)

// globRegexp turns a git "glob" pathspec ("**/" any depth, "/**" everything below, "*" within one
// path segment) into a regular expression source.
func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString(`(?:.*/)?`)
			i += 3
		case strings.HasPrefix(g[i:], "/**"):
			b.WriteString(`/.*`)
			i += 3
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(`.*`)
			i += 2
		case g[i] == '*':
			b.WriteString(`[^/]*`)
			i++
		case g[i] == '?':
			b.WriteString(`[^/]`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
			i++
		}
	}
	return b.String()
}

// pathFilter decides which paths never count: the built-in and user globs, then paths the repo
// marks generated or vendored. Doing this in Go rather than as git pathspecs is far faster,
// because git re-evaluates every pathspec for every file of every commit.
type pathFilter struct {
	dir   string
	re    *regexp.Regexp
	attrs bool
	mu    sync.Mutex
	memo  map[string]bool
}

func newPathFilter(o opts) *pathFilter {
	parts := make([]string, 0, len(excludeGlobs)+len(o.exclude))
	for _, g := range append(append([]string{}, excludeGlobs...), o.exclude...) {
		parts = append(parts, globRegexp(g))
	}
	return &pathFilter{dir: o.dir, re: regexp.MustCompile("^(?:" + strings.Join(parts, "|") + ")$"),
		attrs: usesAttrRules(o.dir), memo: map[string]bool{}}
}

// prime classifies every path not seen before, asking git about attributes in one batch.
func (f *pathFilter) prime(paths []string) {
	f.mu.Lock()
	var todo []string
	for _, p := range paths {
		if _, ok := f.memo[p]; ok {
			continue
		}
		if f.re.MatchString(p) {
			f.memo[p] = true
		} else {
			f.memo[p] = false
			todo = append(todo, p)
		}
	}
	f.mu.Unlock()
	if !f.attrs || len(todo) == 0 {
		return
	}
	cmd := exec.Command("git", "-C", f.dir, "check-attr", "-z", "--stdin", "linguist-generated", "linguist-vendored")
	cmd.Stdin = strings.NewReader(strings.Join(todo, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fields := strings.Split(string(out), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		if v := fields[i+2]; v == "set" || v == "true" {
			f.memo[fields[i]] = true
		}
	}
}

func (f *pathFilter) excluded(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.memo[p]
}

var renameBoth = regexp.MustCompile(`\{([^}]*) => ([^}]*)\}`)

// splitRename returns the old and new path of a numstat entry; both are the path for a plain edit.
func splitRename(path string) (oldP, newP string) {
	if !strings.Contains(path, " => ") {
		return path, path
	}
	if renameBoth.MatchString(path) {
		clean := func(s string) string { return strings.ReplaceAll(s, "//", "/") }
		return clean(renameBoth.ReplaceAllString(path, "$1")), clean(renameBoth.ReplaceAllString(path, "$2"))
	}
	a, b, _ := strings.Cut(path, " => ")
	return a, b
}

type fileStat struct {
	added, deleted int
	path           string
	binary         bool
}

type rawCommit struct {
	d     delta
	files []fileStat
}

// fetchDeltas reads commits oldest first. args select them (date limits and a rev or range).
// The commit list comes from one cheap rev-list, then is split into chunks that git log reads
// in parallel: log is single-threaded and the per-commit diffs dominate the load time.
func fetchDeltas(o opts, args ...string) ([]delta, error) {
	list := append([]string{"rev-list", "--reverse", "--no-merges"}, args...)
	if o.author != "" {
		list = append(list, "--author="+o.author)
	}
	out, err := git(o.dir, list...)
	if err != nil {
		return nil, err
	}
	hashes := strings.Fields(string(out))
	if len(hashes) == 0 {
		return nil, nil
	}
	f := newPathFilter(o)
	n := max(1, min(runtime.NumCPU(), 8, len(hashes)/150))
	size := (len(hashes) + n - 1) / n
	parts := make([][]delta, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		chunk := hashes[i*size : min(len(hashes), (i+1)*size)]
		if len(chunk) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			parts[i], errs[i] = logChunk(o, f, chunk)
		}()
	}
	wg.Wait()
	var ds []delta
	for i := range n {
		if errs[i] != nil {
			return nil, errs[i]
		}
		ds = append(ds, parts[i]...)
	}
	return ds, nil
}

// readLog runs git log over exactly the given commits, in order, and parses the records.
func readLog(o opts, hashes []string, renames bool) ([]rawCommit, error) {
	format := "--format=" + recStart + strings.Join([]string{"%H", "%ct", "%aN", "%aE", "%cN", "%cE",
		"%(trailers:valueonly,unfold,separator=%x1F)", "%(trailers:key=Co-authored-by,valueonly,unfold,separator=%x1F)", "%s", "%b"}, fieldSep) + recEnd
	rn := "-M"
	if !renames {
		rn = "--no-renames"
	}
	cmd := exec.Command("git", "-C", o.dir, "-c", "core.quotepath=off", "log", "--no-walk=unsorted", "--stdin",
		"--no-merges", rn, "--numstat", format, "--", ".")
	cmd.Stdin = strings.NewReader(strings.Join(hashes, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git log: %s", msg)
		}
		return nil, err
	}
	var rs []rawCommit
	for _, rec := range strings.Split(string(out), recStart)[1:] {
		head, rest, _ := strings.Cut(rec, recEnd)
		f := append(strings.SplitN(head, fieldSep, 10), "", "", "", "", "", "", "", "", "", "")
		secs, _ := strconv.ParseInt(f[1], 10, 64)
		d := delta{Hash: f[0], T: time.Unix(secs, 0), Author: f[2], Subject: f[8], Bot: botName.MatchString(f[2]), ByExt: map[string]int{}}
		d.AI = aiIdentity(f[2], f[3], f[4], f[5], f[6]) || aiMessage.MatchString(f[8]+"\n"+f[9])
		if aiIdentity(f[2], f[3]) {
			// an agent authored it: credit the person who launched it, named in a Co-authored-by trailer
			d.Agent = true
			for _, v := range strings.Split(f[7], "\x1f") {
				if name := strings.TrimSpace(strings.SplitN(v, "<", 2)[0]); name != "" && !aiIdentity(v) {
					d.Author, d.Agent = name, false
					break
				}
			}
		}
		rc := rawCommit{d: d}
		for _, line := range strings.Split(rest, "\n") {
			if a, del, p, ok := parseNumstat(line); ok {
				rc.files = append(rc.files, fileStat{added: a, deleted: del, path: p})
			} else if fs := strings.SplitN(line, "\t", 3); len(fs) == 3 && fs[0] == "-" && fs[1] == "-" {
				rc.files = append(rc.files, fileStat{path: fs[2], binary: true}) // binary: no lines, but the commit touched it
			}
		}
		rs = append(rs, rc)
	}
	return rs, nil
}

// logChunk reads the given commits and counts the lines of the files that pass the filter.
// A rename between an excluded and an included path is re-read without rename detection, so the
// included side counts in full, exactly as if the excluded side did not exist.
func logChunk(o opts, f *pathFilter, hashes []string) ([]delta, error) {
	raws, err := readLog(o, hashes, true)
	if err != nil {
		return nil, err
	}
	primeAll := func(rs []rawCommit) {
		var paths []string
		for _, rc := range rs {
			for _, fs := range rc.files {
				a, b := splitRename(fs.path)
				paths = append(paths, a, b)
			}
		}
		f.prime(paths)
	}
	primeAll(raws)
	crossing := func(rc rawCommit) bool {
		for _, fs := range rc.files {
			if a, b := splitRename(fs.path); a != b && f.excluded(a) != f.excluded(b) {
				return true
			}
		}
		return false
	}
	var redo []string
	for _, rc := range raws {
		if crossing(rc) {
			redo = append(redo, rc.d.Hash)
		}
	}
	fixed := map[string]rawCommit{}
	if len(redo) > 0 {
		again, err := readLog(o, redo, false)
		if err != nil {
			return nil, err
		}
		primeAll(again)
		for _, rc := range again {
			fixed[rc.d.Hash] = rc
		}
	}
	ds := make([]delta, 0, len(raws))
	for _, rc := range raws {
		if fx, ok := fixed[rc.d.Hash]; ok {
			rc = fx
		}
		d := rc.d
		kept := false // the commit touches at least one path that is not excluded
		for _, fs := range rc.files {
			_, newP := splitRename(fs.path)
			if f.excluded(newP) {
				continue
			}
			kept = true
			if fs.binary || !o.wants(fs.path) {
				continue
			}
			d.Added += fs.added
			d.Deleted += fs.deleted
			d.ByExt[extOf(fs.path)] += fs.added - fs.deleted
		}
		if kept {
			ds = append(ds, d)
		}
	}
	return ds, nil
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
			win = append(win, commit{Hash: d.Hash, Author: d.Author, Subject: d.Subject, AI: d.AI, Bot: d.Bot, Agent: d.Agent, T: t,
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
		// the ledger starts at the first commit, so the sums are the file sizes as far as commits can tell
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
	return filepath.Join(d, "locomotive", o.key()+".gob")
}

func readCache(o opts) (l ledger) {
	f, err := os.Open(cachePath(o))
	if err != nil {
		return
	}
	defer f.Close()
	if gob.NewDecoder(f).Decode(&l) != nil || l.Detect != detectVersion {
		return ledger{}
	}
	return
}

func writeCache(o opts, l ledger) {
	p := cachePath(o)
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	l.Detect = detectVersion
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
