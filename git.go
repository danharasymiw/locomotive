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
	dir, branch, author, since string
	exts                       map[string]bool // lowercase, no leading dot; empty = all
	exclude                    []string        // extra globs
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
	parts := []string{abs, o.rev(), o.author, o.since, strings.Join(exts, ","), strings.Join(o.exclude, ","), "v3"}
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

type history struct {
	Head    string
	Commits []commit
}

// loadHistory walks commits oldest first. With a prev from the same opts it only reads new commits.
func loadHistory(o opts, prev history) (history, error) {
	out, err := git(o.dir, "rev-parse", o.rev())
	if err != nil {
		return history{}, err
	}
	head := strings.TrimSpace(string(out))
	if prev.Head == head {
		return prev, nil
	}
	rev := head
	if prev.Head != "" {
		if _, err := git(o.dir, "merge-base", "--is-ancestor", prev.Head, head); err == nil {
			rev = prev.Head + ".." + head
		} else {
			prev = history{} // history was rewritten
		}
	}

	args := []string{"log", "--reverse", "--no-merges", "-M", "--format=@%H%x09%ct%x09%aN%x09%(trailers:key=Co-Authored-By,valueonly,unfold,separator=%x1F)%x09%s", "--numstat"}
	if o.author != "" {
		args = append(args, "--author="+o.author)
	}
	if o.since != "" {
		args = append(args, "--since="+o.since)
	}
	args = append(append(args, rev), o.pathspec()...)
	out, err = git(o.dir, args...)
	if err != nil {
		return history{}, err
	}

	commits := append([]commit(nil), prev.Commits...)
	total, by := 0, map[string]int{}
	if n := len(commits); n > 0 {
		total = commits[n-1].Total
		maps.Copy(by, commits[n-1].ByExt)
	}
	var cur *commit
	flush := func() {
		if cur == nil {
			return
		}
		if n := len(commits); n > 0 && cur.T.Before(commits[n-1].T) {
			cur.T = commits[n-1].T // rebased commits can have out-of-order timestamps
		}
		cur.Total, cur.ByExt = total, maps.Clone(by)
		commits = append(commits, *cur)
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
			cur = &commit{Hash: f[0], T: time.Unix(secs, 0), Author: f[2], AI: aiTrailer(f[3]), Subject: f[4]}
		case line != "" && cur != nil:
			if a, d, p, ok := parseNumstat(line); ok && o.wants(p) {
				total += a - d
				by[extOf(p)] += a - d
				cur.Added += a
				cur.Deleted += d
			}
		}
	}
	flush()
	return history{Head: head, Commits: commits}, sc.Err()
}

// --- cache ---

func cachePath(o opts) string {
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "slopmeter", o.key()+".gob")
}

func readCache(o opts) (h history) {
	f, err := os.Open(cachePath(o))
	if err != nil {
		return
	}
	defer f.Close()
	if gob.NewDecoder(f).Decode(&h) != nil {
		return history{}
	}
	return
}

func writeCache(o opts, h history) {
	p := cachePath(o)
	if p == "" || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	tmp := p + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	err = gob.NewEncoder(f).Encode(h)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return
	}
	os.Rename(tmp, p)
}

// loadCached reads new commits on top of prev (or the on-disk cache) and saves the result.
// force ignores both and rebuilds from scratch.
func loadCached(o opts, prev history, force bool) (history, error) {
	if force {
		prev = history{}
	} else if prev.Head == "" {
		prev = readCache(o)
	}
	h, err := loadHistory(o, prev)
	if err == nil && h.Head != prev.Head {
		writeCache(o, h)
	}
	return h, err
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
