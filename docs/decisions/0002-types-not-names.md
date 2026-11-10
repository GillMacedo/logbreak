# 0002: Recognize log calls by type, not by name

**Decision:** each main-module package is type-checked from source with `go/types`, importing dependencies from the export data `go list -export` wrote, and a call counts as a log call only when its callee belongs to slog, zap or logr.

**Why:** loggers live in fields and variables with any name (`s.log`, `r.Log`, `logger`), and many unrelated types have `Info` or `Error` methods. Matching names would both miss calls and report calls that aren't logging. Type-checking also folds constants, so a message held in a `const` compares equal to the same literal.

**Alternatives:** a plain `go/ast` scan is faster but can't tell `logger.Info` from `metrics.Info`. Reading export data alone, as jsonbreak does, gives types but not call sites.
