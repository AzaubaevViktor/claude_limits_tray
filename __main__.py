"""Menu-bar monitor for Claude usage limits.

A circular icon shows two concentric rings (session on the left, weekly on the
right; outer = % left, inner = time since reset) plus a centre Claude spark
whose colour forecasts whether you'll run out of quota at your current rate.
The dropdown spells everything out as numbers and shows the account.

Data comes from the same OAuth endpoint Claude Code's /usage screen uses
(api.anthropic.com/api/oauth/usage); the bearer token is read from the
"Claude Code-credentials" keychain item that Claude Code itself maintains.

Modules: helpers (pure time/format/backoff), api (curl client), forecast
(burn projection + rate tracker), dial (SVG icon), ui (the rumps app).
"""

from ui import ClaudeLimitsApp


def main():
    app = ClaudeLimitsApp()
    try:
        app.run()
    finally:
        app._save_state(force=True)   # backstop if the run loop unwinds


if __name__ == "__main__":
    main()
