package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type worktree struct {
	path, name, branch string
	locked             bool // Claude Code appears to lock worktrees of running agents
}

type wtStat struct {
	wt                    worktree
	delta, added, deleted int
	session               bool // a claude process has its cwd here
	err                   error
}

func (s wtStat) status() string {
	switch {
	case s.session:
		return "session"
	case s.wt.locked:
		return "agent"
	}
	return ""
}

func listWorktrees(dir string) ([]worktree, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []worktree
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			p := line[len("worktree "):]
			wts = append(wts, worktree{path: p, name: filepath.Base(p)})
		case len(wts) == 0:
		case strings.HasPrefix(line, "branch "):
			wts[len(wts)-1].branch = strings.TrimPrefix(line[len("branch "):], "refs/heads/")
		case line == "detached":
			wts[len(wts)-1].branch = "(detached)"
		case line == "locked" || strings.HasPrefix(line, "locked "):
			wts[len(wts)-1].locked = true
		}
	}
	return wts, nil
}

// claudeCwds returns the working directory of every running claude process.
func claudeCwds() []string {
	// lsof -c claude misses it (the process title is a version string), so find pids first.
	pids, _ := exec.Command("pgrep", "-x", "claude").Output()
	list := strings.Join(strings.Fields(string(pids)), ",")
	if list == "" {
		return nil
	}
	out, _ := exec.Command("lsof", "-a", "-p", list, "-d", "cwd", "-Fn").Output()
	var cwds []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			cwds = append(cwds, line[1:])
		}
	}
	return cwds
}

// owner returns the worktree whose root is cwd's git toplevel, or -1. Asking git
// (rather than matching path prefixes) keeps linked worktrees nested inside the
// main one apart, and ignores unrelated repos that happen to live underneath.
func owner(wts []worktree, cwd string) int {
	out, err := git(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return -1
	}
	top := realPath(strings.TrimSpace(string(out)))
	for i, w := range wts {
		if realPath(w.path) == top {
			return i
		}
	}
	return -1
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// pollWorktrees measures uncommitted work in every worktree of the repo, concurrently.
func pollWorktrees(o opts) ([]wtStat, error) {
	wts, err := listWorktrees(o.dir)
	if err != nil {
		return nil, err
	}
	stats := make([]wtStat, len(wts))
	for _, cwd := range claudeCwds() {
		if i := owner(wts, cwd); i >= 0 {
			stats[i].session = true
		}
	}
	var wg sync.WaitGroup
	for i, w := range wts {
		stats[i].wt = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := loadLive(opts{dir: w.path, exts: o.exts, exclude: o.exclude})
			stats[i].delta, stats[i].added, stats[i].deleted, stats[i].err = s.delta(), s.added+s.untracked, s.deleted, err
		}()
	}
	wg.Wait()
	return stats, nil
}
