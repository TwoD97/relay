// Package agentskill embeds Relay's portable agent instructions in the client.
package agentskill

import _ "embed"

//go:embed relay/SKILL.md
var Content string
