# Changelog

All notable changes are documented here, in [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. This project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `--ignore FILE` flag to specify accepted changes and reasons; ignored findings appear in output with an ignored flag and do not fail the run, while stale entries are reported and malformed files exit 2 ([#4](https://github.com/GillMacedo/logbreak/issues/4)).

## [0.1.0] - 2026-11-10

### Added
- Compares the log events of a Go module between two revisions, from `--old DIR --new DIR` or `--base GITREF`, using `go list -export` and the Go type checker.
- Reads log/slog (functions, `*slog.Logger` methods, `Log`, `LogAttrs`, attributes, groups, chained `With`), zap (`*zap.Logger` methods, `Log`, `Check` with `Write`, field constructors, `Namespace`, chained `With`, `SugaredLogger` `w` and `f` methods) and logr (`Info`, `Error`, `V(n)`, chained `WithValues`).
- Findings for removed and reworded messages, removed and renamed keys, value kind changes, and levels lowered or raised, plus new messages and keys.
- `--refs` to search alert rules, dashboards and saved queries for each changed message and key, and mark the ones they use as BREAK.
- `--list` to print the log events of one module, `--pkg`, `--tags`, `--format json`, `--fail-on`, and exit 2 for any input it can't trust.
- `checking-log-event-compatibility` skill and plugin manifest.
