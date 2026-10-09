package transport

import (
	"context"
	"fmt"
	"strings"

	agentskill "github.com/TwoD97/relay/skills"
)

// InstallAgentSkill writes Relay's owned copy and registers it with the two
// supported harnesses. Existing user-authored skills are never overwritten.
func (c *Connection) InstallAgentSkill(ctx context.Context) error {
	output, err := c.Run(ctx, agentSkillInstallScript, strings.NewReader(agentskill.Content))
	if err != nil {
		return fmt.Errorf("Relay skill registration needs attention (existing skills preserved): %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

const agentSkillInstallScript = `set -eu
umask 077
base="$HOME/.local/share/relay"
for dir in "$base/skills" "$base/skills/relay"; do
  if [ -L "$dir" ]; then printf '%s\n' 'Relay skill directory is a symbolic link'; exit 1; fi
  mkdir -p "$dir"
done
dir="$base/skills/relay"
if [ -L "$dir/SKILL.md" ] || { [ -e "$dir/SKILL.md" ] && [ ! -f "$dir/SKILL.md" ]; }; then
  printf '%s\n' 'Relay managed skill file must be a regular file'
  exit 1
fi
tmp=$(mktemp "$dir/.SKILL.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat > "$tmp"
mv -fT "$tmp" "$dir/SKILL.md"
conflict=0
for parent in "$HOME/.agents/skills" "$HOME/.claude/skills"; do
  mkdir -p "$parent"
  target="$parent/relay"
  if [ -L "$target" ] && [ "$(readlink "$target")" = "$dir" ]; then
    continue
  fi
  if [ -e "$target" ] || [ -L "$target" ]; then
    printf 'Existing skill preserved: %s\n' "$target"
    conflict=1
    continue
  fi
  ln -s "$dir" "$target"
done
exit "$conflict"
`
