# slopmeter

A terminal UI that charts a git repo's lines of code over time, per commit and live as you edit. Built to watch how much code (and how much slop) piles up, especially when agents are writing it.

## Install

With Go:

```sh
go install github.com/danharasymiw/slopmeter@latest
```

Or grab a prebuilt binary from the [releases page](https://github.com/danharasymiw/slopmeter/releases).

## Use

```sh
cd your-repo
slopmeter
```

`tab` cycles between three views:

- **history** plots lines of code at every commit. `m` switches between *total*, *languages* (one line per extension, plus a total), *churn* (lines added vs deleted per commit), *authors* (running net lines per person, mailmap-aware) and *human vs ai*. The AI split classifies a commit as AI-assisted when a `Co-Authored-By` trailer names a coding agent (Claude, Copilot, Cursor, Aider, Codex, Gemini and similar), so commits without that trailer count as human.
- **live** plots your working tree (uncommitted changes plus untracked files) once a second.
- **worktrees** plots net uncommitted lines for every worktree of the repo at once. A worktree is tagged `[session]` when a `claude` process is running in it and `[agent]` when git has it locked, which is how Claude Code subagent worktrees appear.

In the history view, move the cursor with `←`/`→` (`shift` jumps 10 commits) or the mouse to see the commit behind each point. `p` cycles the history window (1m, 3m, 6m, 1y, all), `r` rebuilds from scratch, `q` quits.

### Flags

| Flag | Meaning |
| --- | --- |
| `--ext go,ts` | only count these extensions |
| `--exclude '**/*.pb.go'` | extra globs to ignore |
| `--branch main` | branch or ref to walk (default `HEAD`) |
| `--author me` | only commits by this author |
| `--since 6m` | history window: `1m`, `3m` (default), `6m`, `1y`, `all` or a date like `2025-01-01` |
| `--once` | print the chart once and exit (`--mode total\|languages\|churn\|worktrees`) |
| `--json`, `--csv` | print per-commit data and exit |

Lockfiles, vendored dependencies, build output and common generated code (protobuf, minified JS and so on) are ignored by default. See `excludes` in `git.go`.

The commit log is cached once per repo in your user cache directory. Later runs read only new commits, and a wider window reads only the older range it is missing, so a long history is slow only the first time.

## Limits

- Line counts are cumulative added minus deleted lines, not a scan of each checkout, so they're an approximation.
- With `--branch`, `--author` or `--since`, the live view still measures against `HEAD`.
- `[agent]` detection relies on Claude Code locking worktrees of running agents.

## License

MIT
