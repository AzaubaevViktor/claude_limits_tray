# Agent guidelines

A macOS menu-bar app (Go: SVG rendering + `getlantern/systray`) that shows
Claude usage limits. See `README.md` for what it does.

## Commits

- **Every commit message must start with the `[AI]` prefix.**
  Example: `[AI] honour retry-after on 429 backoff`
- **Commit every completed step** — don't batch unrelated changes; once a
  discrete piece of work builds and runs, commit it before moving on.
- Keep the subject line short and imperative; add a body when the change needs it.

## Working on the code

- Files: `helpers.go` (pure time/format/backoff), `api.go` (`Client`),
  `forecast.go` (`Forecast` + `RateTracker`), `dial.go` (`renderDial`),
  `history.go` (the burn charts), `strip.go` (the quota strip above them),
  `app.go` (`App`: poll loop, state, rendering), `menu.go` (menu layout), and a
  thin `main.go`. Keep the pure logic in `helpers`/`forecast` free of GUI deps
  so it stays unit-testable.
- The dial layout is a declarative table — edit the `SEGMENTS` table in
  `dial.go`; the icon and the dropdown legend both follow.
- HTTP goes through the system `curl`, and the token is read from the
  `Claude Code-credentials` keychain item. Don't add network/crypto libraries.
- Requests/responses to the usage API are logged to stderr; never log the token.
- Run it with `./claude_limits.sh` (builds then runs). After edits:
  `go build -o claude-limits . && go test ./...`.
