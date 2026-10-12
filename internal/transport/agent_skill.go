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
	_, err := c.Run(ctx, agentSkillInstallScript, strings.NewReader(agentskill.Content))
	if err != nil {
		return fmt.Errorf("Relay skill registration needs attention (existing skills preserved): %w", err)
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
pending=''
cleanup() {
  rm -f "$tmp"
  if [ -n "$pending" ]; then
    rm -f "$pending/next"
    rmdir "$pending" 2>/dev/null || :
  fi
}
trap cleanup EXIT HUP INT TERM
cat > "$tmp"
mv -fT "$tmp" "$dir/SKILL.md"

# Recognize unmodified bundled registrations from previous installations by
# content, never by their name alone. Only replace the discovery symlink; keep
# the previous installation and a backup of the original link intact.
upgrade_registration() {
  [ -L "$target" ] || return 1
  original=$(readlink "$target")
  previous=$(readlink -f "$target") || return 1
  [ "$previous" != "$dir" ] || return 0
  case "$previous" in "$HOME/.local/share/"*/skills/relay) ;; *) return 1 ;; esac
  namespace=${previous#"$HOME/.local/share/"}
  namespace=${namespace%/skills/relay}
  case "$namespace" in ''|.|..|*/*) return 1 ;; esac
  [ -d "$previous" ] && [ -f "$previous/SKILL.md" ] && [ ! -L "$previous/SKILL.md" ] || return 1
  # Extra files may be user customizations, even when SKILL.md is unchanged.
  [ "$(find "$previous" -mindepth 1 -maxdepth 1 -printf '.')" = '.' ] || return 1
  if ! cmp -s "$previous/SKILL.md" "$dir/SKILL.md"; then
    digest=$(sha256sum < "$previous/SKILL.md") || return 1
    case "${digest%% *}" in
      bc488ebc360d3ff64d8d5c6b04a1b45df07c4786aaa9dd2a331a3063f34528eb) ;;
      *) return 1 ;;
    esac
  fi
  backup=$(mktemp -d "$base/skills/.registration.XXXXXX") || return 1
  ln -s "$original" "$backup/previous" || return 1
  # Stage on the discovery filesystem so replacement remains an atomic rename.
  pending=$(mktemp -d "$parent/.relay-registration.XXXXXX") || return 1
  upgraded=1
  # Recheck because another client may have changed the link while we checked it.
  if ln -s "$dir" "$pending/next" &&
    [ -L "$target" ] && [ "$(readlink "$target")" = "$original" ] &&
    mv -fT "$pending/next" "$target"; then
    upgraded=0
  fi
  rm -f "$pending/next"
  rmdir "$pending"
  pending=''
  return "$upgraded"
}

conflict=0
for parent in "$HOME/.agents/skills" "$HOME/.claude/skills"; do
  mkdir -p "$parent"
  target="$parent/relay"
  if [ -L "$target" ] && [ "$(readlink "$target")" = "$dir" ]; then
    continue
  fi
  if upgrade_registration; then
    continue
  fi
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
