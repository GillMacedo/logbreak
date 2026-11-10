# Examples

All of these are made up for the demo.

- `checkout-v1/` is a payment service that logs with `log/slog`: key-value pairs, `slog.String`, `slog.Int64` and `slog.Duration` attributes, and `WarnContext`.
- `checkout-v2/` changes what it logs on purpose: a reworded message, a renamed key, a removed key, a number logged as a string, and an error lowered to a warning. Run `make demo-break`, which exits 1.
- `checkout-v1-refactor/` rewrites the logging code without changing what it writes: constants instead of literals, `LogAttrs` and `Log`, an empty `slog.Group`, a chained `With`, a helper function, a second file and smaller integer types. Run `make demo-refactor`, which must stay quiet and exit 0 under `--fail-on warn`.
- `alerts/` holds a Loki ruler file and a Grafana dashboard that query the service's logs. They're passed with `--refs`.

The three services are separate modules, so the repo's own `go vet ./...` and `go test ./...` don't build them.
