#!/usr/bin/env sh
# Build (if needed) and run the menu-bar app.
cd "$(dirname "$0")"
go build -o claude-limits . || exit 1
exec ./claude-limits
