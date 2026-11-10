# Security

Report vulnerabilities through [GitHub's private vulnerability reporting](https://github.com/GillMacedo/logbreak/security/advisories/new), not in a public issue.

`logbreak` runs `go list -export` on the modules you point it at, which compiles them (and runs cgo's C compiler on their C code, as `go build` would) without linking or running them. It reads the files you pass with `--refs` and doesn't write anywhere except a temporary directory. With `--base` it runs `git archive` and extracts the archive into that directory, refusing paths that leave it and skipping symlinks. Please report privately if a crafted module, repository or ref file could make it do anything else. A missed change or a wrong finding is a normal bug.
