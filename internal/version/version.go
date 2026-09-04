# Package `internal/version`

Holds the build version string injected at link time by the Makefile
(`-X cs2a/internal/version.Version=...`).

`Version` defaults to `dev` so that locally built binaries report something
meaningful, and tests can assert against a known constant.
