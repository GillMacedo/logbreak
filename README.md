# logbreak: find Go logging changes that break log-based alerts

[![CI](https://github.com/GillMacedo/logbreak/actions/workflows/ci.yml/badge.svg)](https://github.com/GillMacedo/logbreak/actions/workflows/ci.yml) [![MIT license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

**A Claude Code skill and Go CLI for Go backend developers, SREs and reviewers. It compares the log events a Go service writes at two revisions and reports the changes that make Loki, Datadog, CloudWatch or Elasticsearch alerts go quiet: a reworded or removed log message, a renamed or removed attribute key, a value that changed from number to string, and a level that moved from error to warn. It reads `log/slog`, `zap` and `logr` calls.**

Changing `logger.Error("payment failed", ...)` to `logger.Error("charge declined", ...)` compiles, passes review and passes the tests. The alert that counts `msg="payment failed"` stops firing, and nobody notices until an incident. The same happens when `order_id` becomes `orderId`, when `status` goes from `500` to `"500"`, or when an error is downgraded to a warning. `logbreak` reads both revisions with the Go type checker, finds every log call by its real type, and lists what changed. Give it your alert rules or dashboards with `--refs` and it marks each change they use as a BREAK, with the line that uses it.

## What you can ask Claude

- "Does this PR break any of our log-based alerts?"
- "Our 'payment failed' alert went quiet after the last deploy. Did someone change the log line?"
- "I'm migrating this service from zap to slog. Did any message, key or level change?"
- "Which log messages and keys does the order service write, so I can build a dashboard?"

## Install

As a Claude Code plugin:

    claude plugin marketplace add GillMacedo/logbreak
    claude plugin install logbreak@logbreak-marketplace

Or run it directly with Go 1.22 or newer. It has no dependencies:

    go run github.com/GillMacedo/logbreak/plugin/scripts@latest --base main --refs alerts/

## Example

`examples/checkout-v1` is a made-up payment service that logs with `slog`. `examples/checkout-v2` is the next revision, and `examples/alerts` holds two made-up alert files that query its logs: Loki ruler rules and a Grafana dashboard.

    $ make demo-break
    logbreak examples/checkout-v1 -> examples/checkout-v2: 5 log messages compared (slog), 11 call sites
      searched 2 ref files: examples/alerts/checkout-dashboard.json, examples/alerts/payments.rules.yaml
      BREAK key-renamed "invalid amount" order_id
            key "order_id" is now "orderId"
            at payments/charge.go:26
            used in examples/alerts/checkout-dashboard.json:7
      BREAK message-changed "payment failed"
            no longer logged; (*Service).Charge now logs "charge declined" (error), which may be its new wording
            at payments/charge.go:30
            used in examples/alerts/payments.rules.yaml:7
      BREAK level-lowered "refund provider error"
            level lowered from error to warn
            at payments/charge.go:44
            used in examples/alerts/payments.rules.yaml:11
      BREAK kind-changed "refund provider error" status
            value of "status" changed from number to string
            at payments/charge.go:44
            used in examples/alerts/payments.rules.yaml:11
      WARN  key-removed "payment captured" elapsed
            key "elapsed" is no longer logged with this message
            at payments/charge.go:33
      INFO  message-added "charge timing"
            new debug message in (*Service).Charge
            at payments/charge.go:37
      4 breaking, 1 warning, 1 addition

Exit code 1. Four changes are used by the alerts: the rules count `msg="payment failed"`, filter `level="ERROR"` and `status >= 500` on "refund provider error", and the dashboard prints `order_id` for "invalid amount". The removed `elapsed` key isn't used in any ref file, so it's a WARN. The message rename is a guess: "payment failed" disappeared and the same function now logs "charge declined".

`examples/checkout-v1-refactor` rewrites the logging code without changing what it writes: constants instead of literals, `LogAttrs` and `Log` instead of `Error` and `Warn`, a chained `With`, a call moved into a helper, a split file, and `int` changed to `int16`. It stays quiet and exits 0, even with `--fail-on warn`:

    $ make demo-refactor
    logbreak examples/checkout-v1 -> examples/checkout-v1-refactor: 5 log messages compared (slog), 10 call sites
      searched 2 ref files: examples/alerts/checkout-dashboard.json, examples/alerts/payments.rules.yaml
      INFO  key-added "payment captured" currency
            new key "currency" (string)
            at payments/charge.go:37
      no changes to existing log messages, keys or levels (1 addition)

To see the events one revision writes:

    $ make demo-list
    "invalid amount" [warn, slog]
      amount_cents: number
      order_id: string
      at payments/charge.go:25
    "payment captured" [info, slog]
      amount_cents: number
      elapsed: duration
      order_id: string
      at payments/charge.go:32
    "payment failed" [error, slog]
      attempt: number
      err: string
      order_id: string
      at payments/charge.go:29
    "refund issued" [info, slog]
      amount_cents: number
      order_id: string
      at payments/charge.go:46
    "refund provider error" [error, slog]
      order_id: string
      status: number
      at payments/charge.go:43

## Usage

    logbreak (--old DIR | --base GITREF) [--new DIR] [--refs PATH]... [--ignore FILE]
             [--pkg PATTERN]... [--tags T,...] [--format text|json] [--fail-on break|warn|never]
    logbreak --list [--new DIR] [--pkg PATTERN]... [--tags T,...] [--format text|json]

1. **Load both sides.** In each module directory it runs `go list -e -export -compiled -deps -json` (default pattern `./...`), which compiles the packages without linking or running them. It then type-checks the module's own packages from source against the compiler's export data. `--base REF` takes the old side from git with `git archive`, so the new side is your work tree, uncommitted changes included. `--new` defaults to the current directory.
2. **Find log calls.** Calls are recognized by type, so `s.log.Info`, `zap.L().Warn` and `logr.FromContextOrDiscard(ctx).V(2).Info` all count, whatever the variable is called. For each call it records the message, the level and each constant attribute key with the kind of its value.
3. **Compare by message.** Events are grouped by message text across the module, because that's what a log query matches. Keys, value kinds and the highest level are compared per message.
4. **Search the refs.** Each `--refs` file or directory is searched for the changed messages (also JSON-escaped, as in Grafana dashboards) and for changed keys as whole words.

- **`--ignore FILE`** names accepted changes to ignore along with a reason (e.g. `message-changed "payment failed" alert updated in rules.yaml`), preventing them from failing `--fail-on`. Ignored findings still appear in the JSON and text output with an ignored flag and reason. Stale ignore entries are reported, and a malformed file exits 2.
- **`--format json`** prints the same report for scripts and agents, with `severity`, `change`, `message`, `key`, `detail`, `pos` and `refs` per finding.
- **`--fail-on`** sets the lowest severity that exits 1: `warn` by default, `break`, or `never`. `break` needs `--refs`, since without refs nothing can be a BREAK and the run could never fail.
- **`--list`** prints the log events of one module, with levels, keys, value kinds and call sites.
- **`--pkg`** limits the packages, for example to leave out example programs. **`--tags`** passes build tags to `go list`. Other settings such as `GOFLAGS` and `GOWORK` come from the environment.

Exit 0 means no finding at or above `--fail-on`. Exit 1 means findings. Exit 2 means bad usage or a result it can't trust, and it prints nothing on stdout:

- a side that's missing, has no `go.mod`, or doesn't compile (an empty, cut-off or garbage Go file or `go.mod`, or build output in their place), or `go list` output that isn't valid
- a side with no slog, zap or logr call with a constant message, or two sides that are different modules
- a `--base` ref that isn't a commit
- a `--refs` path that is empty, isn't UTF-8 text, is JSON that doesn't parse, has no final newline (so it may be cut off), looks like compiler, `go test` or `go` command output, or mentions no message or key from the old side

Exit 2 ignores `--fail-on`.

## What it reports

| Finding | Severity | Meaning |
|---|---|---|
| `message-removed` | WARN, BREAK if a ref uses it | No call logs this message any more |
| `message-changed` | WARN, BREAK if a ref uses it | The message is gone and the same function now logs a new one. It may be a rewording; the tool can't be sure |
| `key-removed` | WARN, BREAK if a ref uses it | No call with this message logs the key any more |
| `key-renamed` | WARN, BREAK if a ref uses it | One key gone and one key of the same kind added for the same message |
| `kind-changed` | WARN, BREAK if a ref uses it | The value changed between string, number, bool, duration, time, group and array, as in `status` going from `500` to `"500"` |
| `level-lowered`, `level-raised` | WARN, BREAK if a ref uses the message | The highest level of the message changed, such as error to warn, or `V(1)` to `V(4)` in logr |
| `message-added`, `key-added` | INFO | New messages and keys. They never fail the run |

Messages that aren't constants, and attributes that aren't constant keys, are counted on "not checked" lines.

## Supported calls

- **log/slog:** `Debug`, `Info`, `Warn`, `Error`, their `Context` forms, `Log` and `LogAttrs`, as functions and as `*slog.Logger` methods. Key-value pairs, `slog.String`, `Int`, `Int64`, `Uint64`, `Float64`, `Bool`, `Time`, `Duration`, `Any` and `Group` (nested keys as `group.key`, an empty group inlined), and `With` chained onto the call.
- **go.uber.org/zap:** `*zap.Logger` `Debug` to `Fatal`, `Log`, and `Check` followed by `Write` on the same variable. Field constructors such as `zap.String`, `zap.Int64`, `zap.Error`, `zap.Duration` and `zap.Namespace`. `*zap.SugaredLogger` `Infow` and the other `w` methods with key-value pairs, and `Infof` and the other `f` methods, whose format string is the message.
- **github.com/go-logr/logr:** `Info`, `Error` (which adds the `error` key), `V(n)` chains, and chained `WithValues`.
- `fmt.Sprintf` with a constant format as a message is compared by its format string.

## How it compares

- **`sloglint`** (go-simpler, MPL-2.0) and other slog and zap linters check that one revision follows a style: static messages, constant keys, key naming, no mixed arguments. They don't compare revisions or know about alerts.
- **`go vet`'s `slog` check** catches malformed key-value pairs in one revision.
- **Log platforms** can alert when a pattern stops appearing, after the deploy. `logbreak` runs before the merge.
- **A stable event field** (an `event="payment_failed"` attribute that alerts use instead of message text) is the long-term fix many teams adopt. `logbreak` also catches a change to that key or its value kind, but not to its value, because values are run-time data.

I searched GitHub and the web for a tool that compares log messages, keys or levels between two revisions of Go code and found none. Keyword search can miss projects.

## Limits

- **Both revisions must compile**, with their dependencies in the module cache or downloadable.
- **Wrappers hide calls.** A project function such as `func logError(msg string, args ...any)` that calls slog inside is seen once, with a message it can't read, and its callers aren't seen. Calls through an interface, `slog.Handler` code, `logrus`, `zerolog`, `klog` and the standard `log` package aren't read.
- **Attributes added away from the call aren't tracked**: a logger stored with `With` in a struct field, keys built in a helper, `args...` spreads and `WithGroup` prefixes. These calls are counted on the "not checked" line, and a removed key on such a call says so.
- **Messages are matched across the whole module.** If two packages log the same message and one stops, nothing is reported, because a query for the message still matches. A message added in an example program is still a new call site; use `--pkg` to leave those out.
- **Ref search is plain text.** A regex in a query such as `|~ "payment.*failed"` isn't expanded, and a key that's a common word (`error`, `id`) matches many lines, so more changes become BREAK than strictly need to. A ref file cut exactly at a line break can't be told from a complete one.
- **Values aren't compared.** `status="declined"` changing to `status="rejected"` is invisible, because the value is decided at run time.
- **Handlers change the output.** `slog`'s JSON handler writes `msg` and `level="ERROR"`, zap's production config writes `msg` and `level="error"`; renaming those top-level fields in a handler config isn't seen.

## FAQ

**How do I know if a code change broke a log-based alert?** Run `logbreak --base main --refs path/to/alerts` in the module root, or ask the skill to check the pull request. Each change an alert uses is a BREAK with the alert's file and line.

**Is changing a log message a breaking change?** For anything that queries logs by message text, yes: Loki and Elasticsearch queries, Datadog log monitors, CloudWatch metric filters and saved searches stop matching. `logbreak` treats a removed or reworded message as a WARN, and as a BREAK when a ref file uses it.

**Does lowering a log level from error to warn break alerts?** Alerts that filter on `level="error"` or count errors stop seeing it. It's reported as `level-lowered`.

**Can I use it without alert files?** Yes. Every change to an existing message, key, kind or level is a WARN and the run exits 1. `--fail-on break` needs `--refs`.

**Does it run my code or tests?** No. `go list -export` compiles packages to export data without linking or running anything. cgo packages run the C compiler, as `go build` does.

**Does it need network access?** No, as long as the dependencies of both revisions are in the module cache.

**Is it an MCP server?** No. It's a skill with one standard-library Go file, so `go run` is the whole install.

## Tested on

- Unit tests for template literals, whole-word and query-position key matching, zap field kinds, damaged ref files, JSON-escaped messages, flag errors and the archive extractor, which refuses paths that leave its directory and archives that are cut off.
- Integration tests in `tests/` that build the tool and run it on the three examples, on each example against itself, on 18 before-and-after slog modules, and on 13 zap and logr modules (fetched into the module cache; skipped if that isn't possible). Each set has changes that must be reported and close cases that must stay quiet: a constant instead of a literal, `Info` to `InfoContext` or `LogAttrs`, a chained `With`, `int` to `int64`, reordered keys, a call moved to another function, zap `Check` instead of a direct call, logr `V(0)` instead of `Info`.
- Damaged inputs, on the old side and the new side in turn with the other side real: a missing directory, an empty directory, an empty, garbage, cut-off and build-log `go.mod`, a module with no packages, an empty Go file, garbage, a Go file cut off partway, `go build` and `go test` failure output in place of a Go file, a type error, a module with no log calls, and one with only non-constant messages. Each exits 2 with nothing on stdout, with the default `--fail-on`, `--fail-on never`, `--fail-on warn`, `--fail-on break` with refs, and JSON output. A Go file cut off between two functions still compiles; its lost calls are reported as removed (exit 1), and as BREAK when a ref uses them. A cut-off `go.mod` that still parses names another module and exits 2.
- Damaged `--refs`, alone and next to the good alert files: empty, binary garbage, text that mentions nothing from the code, cut-off JSON, cut-off YAML, compiler output, `go test` failure output, `go` command errors, an empty directory, a directory of unrelated rules and a missing path. Each exits 2 with every `--fail-on`, including when the code didn't change. A fake `go` command printed nothing, garbage, cut-off JSON, a build log, a failure, JSON pointing to missing export data, a package error and JSON with only dependencies; all exit 2.
- `--base` in a real git repository with the module in a subdirectory: HEAD against an unchanged tree, a tag against a changed tree, unknown refs, a ref that looks like a flag, a commit without the module, and a directory outside git.
- Go 1.22.12 and Go 1.27.3 on linux/arm64, with `-race`. The CI workflow runs the same tests on linux/amd64 with Go 1.22 and the latest stable Go.

Run on real releases, from module zips, on 2026-11-10, with no ref files:

- **caddy v2.8.4 to v2.9.0** (zap, 264 messages): 8 WARN findings, each checked against the source. `aborting with incomplete response` went from `Error` to a `Check` at `WarnLevel`. Two `redirecting to canonical URI (...)` messages lost their closing parenthesis when they moved to `Check`, which breaks an exact-match query. `active health check panicked`, `getting remote IP` and a deprecation warning are no longer logged, and an HTTP/3 warning was replaced by another one without its `file` key.
- **caddy v2.9.0 to v2.9.1**, a patch release: quiet, exit 0, with one new message.
- **controller-runtime v0.19.0 to v0.20.0** (logr): the reconciler warning whose typo `reqeueuing` was fixed, the certificate watcher's start message reworded, and two webhook builder messages removed. A new `Reconciling` call at info level in an example program is reported as `level-raised` with its location; `--pkg ./pkg/...` leaves it out.
- **alertmanager v0.28.0 to v0.28.1** (slog, 141 messages): quiet, exit 0.

Not tested: `--refs` against real alert rules from a production system (only the made-up files in `examples/alerts`), Windows or macOS as the machine running the tool, very large monorepos, and use in a fresh Claude session.

## Contributing

Contributions are welcome, from people and from people working with AI agents. See [CONTRIBUTING.md](CONTRIBUTING.md). Good first issues are [listed here](https://github.com/GillMacedo/logbreak/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22).

### How pull requests work

1. **Open a PR.** Fork the repo, work on a branch, and keep it to one change. Write a title that says what changes for users, because it becomes the changelog line: `feat:` goes under Added and `fix:` under Fixed.
2. **Automatic check.** Code changes need tests, and `make test` must pass. Changes over 30 lines need a linked open issue (`Closes #<number>`), and a PR can have at most 800 changed lines. If something is missing, a comment tells you what to fix.
3. **Review.** Claude reviews each PR automatically. Code PRs also get a security review and are tested in a sandbox. The review then merges the PR or asks for changes, and a small problem may become a follow-up issue instead. A PR with no new commit 14 days after a change request is closed, and you can reopen it. PRs that change CI, agent settings, npm or Python dependencies, vendored code or build files can't be merged this way, and a comment says so. The same goes for anything the security review doesn't clear.
4. **Release.** A merge that changes what users install becomes a new version within about two hours, with a GitHub Release and, if the project has one, its npm package. Docs and CI changes wait for the next release.

## AI disclosure

This project was largely generated with Claude Code and reviewed by its maintainer.

## About me

I'm Giulliano Macedo, and I build developer tools at GoTech Solutions. Follow along or get in touch:

- GitHub: [GillMacedo](https://github.com/GillMacedo)
- LinkedIn: [giulliano](https://www.linkedin.com/in/giulliano/)
- X: [@giumacedo](https://x.com/giumacedo)

If `logbreak` saved you some prompts, a star helps others find it.

---

Built by [Giulliano Macedo](https://github.com/GillMacedo), GoTech Solutions · [LinkedIn](https://www.linkedin.com/in/giulliano/) · [X](https://x.com/giumacedo)
