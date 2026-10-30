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

- **history** plots lines of code at every commit. `m` switches between *total*, *languages* (one line per extension, plus a total) and *churn* (lines added vs deleted per commit).
- **live** plots your working tree (uncommitted changes plus untracked files) once a second.
- **worktrees** plots net uncommitted lines for every worktree of the repo at once. A worktree is tagged `[session]` when a `claude` process is running in it and `[agent]` when git has it locked, which is how Claude Code subagent worktrees appear.

In the history view, move the cursor with `←`/`→` (`shift` jumps 10 commits) or the mouse to see the commit behind each point. `r` rebuilds from scratch, `q` quits.

### Flags

| Flag | Meaning |
| --- | --- |
| `--ext go,ts` | only count these extensions |
| `--exclude '**/*.pb.go'` | extra globs to ignore |
| `--branch main` | branch or ref to walk (default `HEAD`) |
| `--author me` | only commits by this author |
| `--since 6.months` | only commits since this date |
| `--once` | print the chart once and exit (`--mode total\|languages\|churn\|worktrees`) |
| `--json`, `--csv` | print per-commit data and exit |

Lockfiles, vendored dependencies, build output and common generated code (protobuf, minified JS and so on) are ignored by default. See `excludes` in `git.go`.

History is cached in your user cache directory and only new commits are read on later runs.

## Limits

- Line counts are cumulative added minus deleted lines, not a scan of each checkout, so they're an approximation.
- With `--branch`, `--author` or `--since`, the live view still measures against `HEAD`.
- `[agent]` detection relies on Claude Code locking worktrees of running agents.

## License

MIT
