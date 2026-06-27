# CLAUDE.md

Guidance for Claude when working in this repo. See `AGENTS.md` for the full
conventions; the essentials are repeated here.

## Commits

- **Commit after every completed step.** Once a discrete piece of work builds
  (`go build -o claude-limits . && go test ./...`) and runs, commit it before
  moving on — don't batch unrelated changes into one commit.
- **Every commit message must start with the `[AI]` prefix**, short and
  imperative. Example: `[AI] honour retry-after on 429 backoff`.

## Building

- `go build -o claude-limits . && go test ./...` after edits.
- Run with `./claude_limits.sh` (builds then runs).
