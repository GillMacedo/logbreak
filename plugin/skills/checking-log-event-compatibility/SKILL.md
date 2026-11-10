---
name: checking-log-event-compatibility
description: Compares the log events a Go module writes at two revisions (a git ref or two directories) and reports changes that silently break log-based alerts, dashboards and saved searches, such as a reworded or removed log message, a renamed or removed attribute key, a value that changed from number to string, and a level that went from error to warn. Reads log/slog, go.uber.org/zap (Logger, Check and SugaredLogger) and github.com/go-logr/logr calls. With alert rule, dashboard or query files it marks the changes they use as BREAK. Use when reviewing a Go pull request that touches logging, after a log-based alert went quiet following a deploy, or before a release of a Go service whose logs feed Loki, Datadog, CloudWatch, Elasticsearch or Splunk.
---

# Checking Go log event compatibility

The bundled tool needs Go 1.22 or newer, and git for `--base`. It runs `go list -export` on each revision, which compiles packages without linking or running them, then type-checks the module's own packages from source to find log calls by their real types. It never runs the project's code.

## 1. Pick the two revisions

- In a pull request, the old side is the merge base: `git merge-base origin/main HEAD`. Use the branch name only if the user asks for it.
- For a release, use the last tag (`git describe --tags --abbrev=0`) against the work tree.
- `--base REF` reads the old side with `git archive`, so uncommitted changes in the work tree are the new side. Use `--old DIR --new DIR` for two checkouts.
- Run it in the module root (the directory with `go.mod`). In a repo with several modules, run it once per module.

## 2. Find the alert rules, if the repo has them

Look for files that query logs: Loki or Prometheus rule files (`*.rules.yaml`, `alerts/`), Grafana dashboards (`*.json` with `expr`), Datadog or Terraform monitor definitions, CloudWatch metric filters, saved searches. Pass each file or directory with `--refs`. If you find none, run without `--refs` and say that nothing could be marked BREAK.

## 3. Run it

    go run "${CLAUDE_PLUGIN_ROOT}/scripts/logbreak.go" --base "$(git merge-base origin/main HEAD)" --new MODULE_DIR --refs PATH

- Add `--pkg ./cmd/... --pkg ./internal/...` to leave out example programs or tools whose logs nobody alerts on.
- Pass `--tags` if logging code sits behind build tags, and set `GOFLAGS` as CI does.
- Add `--format json` to parse the result.
- `--list --new DIR` prints every log message with its levels, keys and value kinds, which helps you explain a finding or write a new alert.
- `--fail-on break` only works with `--refs`, since only a change a ref file mentions is a BREAK.

## Exit codes

0 means no finding at or above `--fail-on` (default `warn`). 1 means findings. 2 means the result can't be trusted: a side doesn't build, has no slog, zap or logr call with a constant message, the two sides are different modules, a `--base` ref doesn't exist, or a `--refs` path is empty, cut off, not text, looks like build output, or mentions no message or key from the old side. Treat 2 as unknown, never as a pass, and show the user the error.

## Report

1. List each BREAK with the message, key, file line and the ref line that uses it. Say what happens: the alert stops matching (message or key gone), the comparison changes (number became string), or the alert stops seeing it (error became warn).
2. For WARN findings, say that no ref file you were given mentions them, so the risk depends on queries outside the repo, such as saved searches in the logging tool.
3. A `message-changed` finding is a guess: a message disappeared and the same function logs a new one. Check the code before you say it was reworded.
4. Say what wasn't checked, from the "not checked" lines: messages that aren't constants, attributes passed through variables or helpers, and loggers wrapped in the project's own functions.
5. INFO lines are additions. Mention them only if the user asks.
6. Don't edit code or alert rules unless the user asks. If they do, the usual fix is to keep the old message or key, or update the alert in the same pull request.
