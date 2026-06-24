# Agent guidelines

A macOS menu-bar app (rumps + pyobjc) that shows Claude usage limits. See
`README.md` for what it does and `__main__.py` for the implementation.

## Commits

- **Every commit message must start with the `[AI]` prefix.**
  Example: `[AI] honour retry-after on 429 backoff`
- **Commit every completed step** — don't batch unrelated changes; once a
  discrete piece of work builds and runs, commit it before moving on.
- Keep the subject line short and imperative; add a body when the change needs it.

## Working on the code

- Single module, `__main__.py`, organised into classes: `UsageClient`,
  `Forecast`, `RateTracker`, `Dial`, and `ClaudeLimitsApp(rumps.App)`.
- The dial layout is a declarative table — edit `Dial.SEGMENTS`; the icon and
  the dropdown legend both follow.
- No third-party deps beyond `rumps` (which pulls pyobjc); HTTP goes through the
  system `curl`, and the token is read from the `Claude Code-credentials`
  keychain item. Don't add network/crypto libraries for this.
- Run it with `./claude_limits.sh`; quick sanity check after edits:
  `\.venv/bin/python -c "import py_compile; py_compile.compile('__main__.py', doraise=True)"`
