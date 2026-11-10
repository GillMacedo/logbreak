# logbreak

Claude Code skill and Go CLI that compares the log events (messages, attribute keys, value kinds, levels) a Go module writes at two revisions, for log/slog, zap and logr, and reports changes that break log-based alerts, marking the ones a given alert rule or dashboard file uses.

## Architecture
`plugin/scripts/logbreak.go` holds all the logic in one standard-library file, so `go run` needs no setup. `loadSide` runs `go list -e -export -compiled -deps -json` in each module directory, and `checkPackage` type-checks each main-module package from source with `go/types`, importing dependencies from export data. `visit` recognizes log calls by the callee's package and receiver type; `slogCall`, `zapCall`, `zapCheck`, `sugarCall` and `logrCall` read the message, level and keys. `groupEvents` groups calls by message, `diffEvents`, `diffKeys` and `diffLevel` produce findings, and `loadRefs`, `checkRefsRelevant`, `findMessage` and `findKey` handle `--refs`. `extractGitRev` reads `--base` with `git archive`. `plugin/skills/checking-log-event-compatibility/SKILL.md` tells Claude how to use it. See `docs/architecture.md`.

## Commands
- `make test`: gofmt, vet, unit tests and the integration tests in `tests/`
- `make lint`: plugin and marketplace manifest validation
- `make demo`: the README examples on `examples/checkout-v1`, `checkout-v2`, `checkout-v1-refactor` and `alerts`

## Conventions
- Standard library only. Never run the code being checked; `go list -export` and `git archive` are the only commands it runs.
- An input it can't fully trust (a side that doesn't build, `go list` output that isn't valid, no log calls, two different modules, a bad `--base` ref, a `--refs` path that is empty, cut off, not text, build output or unrelated) exits 2 and prints nothing on stdout, whatever `--fail-on` says.
- Something the tool can't see (a message that isn't a constant, attributes passed through variables) is a "not checked" line or a note on the finding, never silence.
- A new call pattern needs a case in `tests/libraries_test.go` that must be reported and a close case that must stay quiet.
- Tests must not assume the host GOOS or GOARCH.

## Do and don't
- Do keep README examples as real output from `make demo-break`, `make demo-refactor` and `make demo-list`.
- Don't claim a change is safe for every query. The tool reports what changed in the code and which given ref files mention it; queries kept elsewhere are the user's call.

## Definition of done
`make test`, `make lint` and `make demo` pass, and `ROADMAP.md`, `README.md` and docs are updated when relevant.
