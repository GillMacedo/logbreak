# Roadmap

## Next
- zerolog (`log.Info().Str("k", v).Msg("...")`) and logrus (`WithField`, `WithFields`)
- Follow a logger stored with `With` in a struct field or a local variable, so its keys count for later calls
- An `--ignore` file for accepted changes, with a reason per line
- A GitHub Action example that runs `logbreak --base` against the pull request's merge base and posts nothing without a person approving it

## Later
- Treat the project's own logging wrappers as log calls, from a list of function names
- klog structured calls (`InfoS`, `ErrorS`) and the standard `log` package
- Read regular expressions in LogQL and Elasticsearch queries instead of plain text search
