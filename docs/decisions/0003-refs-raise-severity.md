# 0003: Alert files raise severity; they don't decide what's reported

**Decision:** every change to an existing message, key, kind or level is reported. A change that a `--refs` file mentions is a BREAK, and the rest are WARN. The default `--fail-on` is `warn`, and `--fail-on break` requires `--refs`.

**Why:** most queries on logs live outside the repository, in Datadog, Kibana or a saved search, so a change no ref file mentions can still break something. Making refs required, or hiding changes they don't mention, would make a run look safe when the tool simply couldn't see the query.

**Refusing unrelated refs:** a ref path that mentions no message or key from the old side is refused with exit 2. Otherwise a wrong path, an empty export or a pasted build log would turn every BREAK into a WARN, and a run with `--fail-on break` would pass.
