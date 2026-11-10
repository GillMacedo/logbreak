# Architecture

    parse flags
      --base REF: git rev-parse, git archive REF | untar to a temp dir (old side)
      --> for each side: go list -e -export -compiled -deps -json PATTERNS
            any Error, DepsErrors, missing export data, bad JSON or exit != 0 --> exit 2
          for each main-module package: parse CompiledGoFiles, go/types.Check with
            go/importer.ForCompiler("gc", lookup) for imports; parse or type error --> exit 2
          zapCheckWrites: link each `ce := logger.Check(...)` to the ce.Write(...) calls
          visit every call: callee package + receiver type
            log/slog            --> slogCall (message, level, key-value pairs and Attrs)
            zap Logger          --> zapCall, zapCheck (fields)
            zap SugaredLogger   --> sugarCall (w: key-value pairs, f: format string)
            logr Logger         --> logrCall (V(n) chain, Error adds "error")
            withKeys: With / WithValues chained onto the receiver
          non-constant message --> counted as not checked
      --> no constant-message calls on a side, or two different modules --> exit 2
      --> loadRefs: each file must be non-empty UTF-8 text, valid JSON for .json,
            end in a newline otherwise, and not look like build or test output --> else exit 2
          checkRefsRelevant: each --refs path must mention an old message or key --> else exit 2
      --> groupEvents: calls grouped by message; keys (with kinds), highest level, partial flag
      --> diffEvents:
            message only in old: paired with a new message in the same function --> message-changed
                                 else --> message-removed
            message in both: diffKeys (removed, renamed, kind-changed, added), diffLevel
            message only in new --> message-added
            each finding: refs mention the message or key --> BREAK, else WARN
      --> text or JSON --> exit code

- **The type checker decides what's a log call.** A call counts when its callee is a function or method of `log/slog`, `go.uber.org/zap` or `github.com/go-logr/logr`, whatever the receiver variable is called. Constants are folded by `go/types`, so `const msg = "x"` and `"x"` are the same message.
- **Events are keyed by message text**, because log queries match on it. Keys, kinds and levels are merged over every call with that message, so moving a call between functions or files is quiet, and the same message logged from two places is one event.
- **Kinds are broad.** A value is a string, number, bool, duration, time, group, array or any. `int` to `int64` is still a number; `error` and `string` are both strings because handlers write errors as text. `any` never produces a kind change.
- **Refs only raise severity.** Without `--refs`, every change is a WARN. A ref file that mentions nothing from the old side is refused, so a wrong path or a pasted build log can't make a run look safe.
- **Untrusted input stops the run.** Every failure to load a side or a ref exits 2 before any output is printed.

Key choices are in `decisions/`.
