# logbreak plugin

This plugin adds the `checking-log-event-compatibility` skill and the `scripts/logbreak.go` tool. The tool compares the log events a Go module writes at two revisions: messages, attribute keys, value kinds and levels from log/slog, zap and logr calls. It reports changes that break log-based alerts and dashboards, and marks as BREAK the ones that the alert rule or dashboard files you pass with `--refs` use. It needs Go 1.22 or newer, and git for `--base`. See the repository README for usage, findings, limits and what was tested.
