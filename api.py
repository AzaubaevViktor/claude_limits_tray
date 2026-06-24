"""OAuth usage/profile API client (via the system curl)."""

import json
import subprocess


class ApiError(Exception):
    """HTTP/transport failure talking to the OAuth API."""

    def __init__(self, message, status=None, retry_after=None, reset_at=None):
        super().__init__(message)
        self.status = status
        self.retry_after = retry_after   # seconds, from the retry-after header
        self.reset_at = reset_at         # ISO/epoch, from a ratelimit-reset header


class UsageClient:
    """Reads the keychain token and calls the OAuth endpoints via system curl
    (curl uses the macOS trust store — more portable than Python's OpenSSL)."""

    BASE = "https://api.anthropic.com"
    KEYCHAIN = "Claude Code-credentials"
    USER_AGENT = "claude-cli/2.1.187 (external, cli)"
    OAUTH_BETA = "oauth-2025-04-20"
    META = "\n__META__"

    def _token(self):
        try:
            out = subprocess.check_output(
                ["security", "find-generic-password", "-s", self.KEYCHAIN, "-w"],
                text=True, stderr=subprocess.DEVNULL)
            oauth = json.loads(out)
            return oauth.get("claudeAiOauth", oauth)["accessToken"]
        except Exception as exc:
            raise ApiError("No credentials — sign in to Claude Code") from exc

    def _get(self, path):
        token = self._token()
        proc = subprocess.run(
            ["curl", "-sS", "--max-time", "15",
             "-H", f"Authorization: Bearer {token}",
             "-H", f"anthropic-beta: {self.OAUTH_BETA}",
             "-H", f"User-Agent: {self.USER_AGENT}",
             "-H", "Accept: application/json",
             "-w", f"{self.META}%{{http_code}}|%header{{retry-after}}"
                   "|%header{anthropic-ratelimit-unified-reset}",
             self.BASE + path],
            capture_output=True, text=True)
        if proc.returncode != 0:
            raise ApiError("Offline — can't reach Claude API")
        body, _, meta = proc.stdout.rpartition(self.META)
        fields = meta.split("|")
        try:
            status = int(fields[0])
        except (ValueError, IndexError):
            raise ApiError("Malformed curl response")
        if status >= 400:
            retry_after = None
            retry_s = fields[1].strip() if len(fields) > 1 else ""
            if retry_s:
                try:
                    retry_after = int(float(retry_s))
                except ValueError:
                    retry_after = None
            reset_at = (fields[2].strip() or None) if len(fields) > 2 else None
            raise ApiError(f"HTTP {status}", status=status,
                           retry_after=retry_after, reset_at=reset_at)
        return json.loads(body)

    def usage(self):
        return self._get("/api/oauth/usage")

    def profile(self):
        return self._get("/api/oauth/profile")
