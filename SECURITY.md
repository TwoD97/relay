# Security

Relay is a personal, loopback-only control plane. Do not expose its controller
directly to the public internet. SSH host trust and provider permission prompts
remain part of the connection and agent workflows.

Approval decisions require an authenticated client and exact pending request
identity; they never alter provider policy. Optional session summaries send
bounded agent terminal excerpts to the configured provider through the host's
own signed-in CLI. They are disabled by default. See [Activity](docs/ACTIVITY.md)
for hook trust, retained data, worker isolation, and compatibility limits.

Please report vulnerabilities through this repository's private security
advisory reporting feature. Include the affected version, a reproducible example
using disposable hosts, and the expected security boundary. Do not include real
credentials or private host information in a public issue.

Only the current development branch is maintained until a stable release policy
is published. Downloaded agent tools are separate products with their own
authentication, update, and security policies.
