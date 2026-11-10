.PHONY: test lint demo demo-break demo-refactor demo-list tool

test:
	test -z "$$(gofmt -l plugin tests examples)"
	go vet ./...
	cd examples/checkout-v1 && go vet ./...
	cd examples/checkout-v2 && go vet ./...
	cd examples/checkout-v1-refactor && go vet ./...
	go test -race -count=1 ./...

lint:
	claude plugin validate ./plugin --strict
	claude plugin validate . --strict

TOOL := bin/logbreak

tool:
	@go build -o $(TOOL) ./plugin/scripts

demo: demo-break demo-refactor demo-list

# checkout-v2 changes log events that the example alerts use, so exit 1 is the expected result.
demo-break: tool
	@$(TOOL) --old examples/checkout-v1 --new examples/checkout-v2 --refs examples/alerts || test $$? -eq 1

# checkout-v1-refactor rewrites the logging code but keeps every message, key and level.
# It must stay quiet and exit 0, even with --fail-on warn.
demo-refactor: tool
	@$(TOOL) --old examples/checkout-v1 --new examples/checkout-v1-refactor --refs examples/alerts --fail-on warn

demo-list: tool
	@$(TOOL) --list --new examples/checkout-v1
