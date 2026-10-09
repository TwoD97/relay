# Contributing

Open an issue to describe a bug or discuss a substantial change, then send a pull
request with the behavior changed and the checks run. Keep changes focused and
include a regression test when changing runtime, authentication, persistence, or
session lifecycle behavior.

Build and test instructions are in [README.md](README.md) and
[desktop/README.md](desktop/README.md). The [API contract](docs/CONTRACT.md) explains
the boundary between the controller, runtime, and clients.

Use disposable local state and SSH test containers. Keep private hosts, keys,
provider credentials, terminal history, and generated artifacts out of commits
and issue reports. Sanitize diagnostics before sharing them.

Contributions are provided under the project's Apache-2.0 license.
