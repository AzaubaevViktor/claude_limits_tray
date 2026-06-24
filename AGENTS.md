# Agent guidelines

A macOS menu-bar app (rumps + pyobjc) that shows Claude usage limits. See
`README.md` for what it does.

## Commits

- **Every commit message must start with the `[AI]` prefix.**
  Example: `[AI] honour retry-after on 429 backoff`
- **Commit every completed step** — don't batch unrelated changes; once a
  discrete piece of work builds and runs, commit it before moving on.
- Keep the subject line short and imperative; add a body when the change needs it.

## Working on the code

- Modules: `helpers` (pure time/format/backoff), `api` (`UsageClient`),
  `forecast` (`Forecast` + `RateTracker`), `dial` (`Dial` icon), `ui`
  (`ClaudeLimitsApp`), and a thin `__main__.py`. Keep the pure logic in
  `helpers`/`forecast` free of GUI imports so it stays unit-testable.
- The dial layout is a declarative table — edit `Dial.SEGMENTS`; the icon and
  the dropdown legend both follow.
- No third-party runtime deps beyond `rumps` (which pulls pyobjc); HTTP goes
  through the system `curl`, and the token is read from the
  `Claude Code-credentials` keychain item. Don't add network/crypto libraries.
- Run it with `./claude_limits.sh`. After edits: `.venv/bin/python -m pytest -q`.
