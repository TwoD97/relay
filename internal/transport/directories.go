package transport

import (
	"bytes"
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const DirectoryLimit = 200
const directoryOutputLimit = 128 << 10

var (
	ErrDirectoryNotFound   = errors.New("folder does not exist")
	ErrDirectoryPermission = errors.New("permission denied opening this folder")
	ErrNotDirectory        = errors.New("path is not a folder")
)

type DirectoryQuery struct {
	Path   string
	Prefix string
	Hidden bool
}

type DirectoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirectoryListing struct {
	Home        string           `json:"home"`
	Path        string           `json:"path"`
	Parent      *string          `json:"parent"`
	Directories []DirectoryEntry `json:"directories"`
	Truncated   bool             `json:"truncated"`
}

func (q DirectoryQuery) Validate() error {
	if len(q.Path) > 4096 || !utf8.ValidString(q.Path) || strings.ContainsAny(q.Path, "\x00\r\n") ||
		(q.Path != "" && q.Path != "~" && !strings.HasPrefix(q.Path, "~/") && !strings.HasPrefix(q.Path, "/")) {
		return errors.New("folder path must be an absolute path or ~/path without line breaks")
	}
	if len(q.Prefix) > 255 || !utf8.ValidString(q.Prefix) || strings.ContainsAny(q.Prefix, "/\x00\r\n") {
		return errors.New("folder prefix must be a single name without line breaks")
	}
	return nil
}

func (q DirectoryQuery) Matches(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsAny(name, "/\x00\r\n") &&
		(q.Hidden || strings.HasPrefix(q.Prefix, ".") || !strings.HasPrefix(name, ".")) &&
		strings.HasPrefix(name, q.Prefix)
}

// ListDirectories needs only the established SSH connection and Linux core
// utilities. It does not depend on, upload, activate, or restart a runtime.
func (c *Connection) ListDirectories(ctx context.Context, q DirectoryQuery) (DirectoryListing, error) {
	if err := q.Validate(); err != nil {
		return DirectoryListing{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.Run(ctx, directoryScript(q), nil)
	if err != nil {
		return DirectoryListing{}, err
	}
	return parseDirectoryOutput(out, q)
}

func directoryScript(q DirectoryQuery) string {
	// find's -name consumes a glob, so escape its metacharacters before adding
	// the only intended wildcard. Shell quoting is a separate necessary step.
	pattern := strings.NewReplacer(`\`, `\\`, "*", `\*`, "?", `\?`, "[", `\[`).Replace(q.Prefix) + "*"
	hidden := " ! -name '.*'"
	if q.Hidden || strings.HasPrefix(q.Prefix, ".") {
		hidden = ""
	}
	return `set -eu
requested=` + quoteShell(q.Path) + `
case "$requested" in
  ''|'~') requested="$HOME" ;;
  '~/'*) requested="$HOME/${requested#??}" ;;
esac
fail() { printf 'relay-directories-v1\000%s\000' "$1"; exit 0; }
[ -e "$requested" ] || fail not_found
[ -d "$requested" ] || fail not_directory
[ -r "$requested" ] && [ -x "$requested" ] || fail permission
cd -P "$requested" 2>/dev/null || fail permission
command -v find >/dev/null && command -v head >/dev/null || fail unavailable
printf 'relay-directories-v1\000ok\000%s\000%s\000' "$HOME" "$PWD"
LC_ALL=C find -L . -mindepth 1 -maxdepth 1 -type d -name ` + quoteShell(pattern) + hidden + ` -print0 2>/dev/null | head -c 131073
`
}

func parseDirectoryOutput(out []byte, q DirectoryQuery) (DirectoryListing, error) {
	parts := bytes.SplitN(out, []byte{0}, 5)
	if len(parts) < 3 || string(parts[0]) != "relay-directories-v1" {
		return DirectoryListing{}, errors.New("host returned an invalid folder listing")
	}
	switch string(parts[1]) {
	case "not_found":
		return DirectoryListing{}, ErrDirectoryNotFound
	case "not_directory":
		return DirectoryListing{}, ErrNotDirectory
	case "permission":
		return DirectoryListing{}, ErrDirectoryPermission
	case "ok":
	default:
		return DirectoryListing{}, errors.New("folder browsing requires Linux find and head utilities")
	}
	if len(parts) != 5 || !validDirectoryPath(string(parts[2])) || !validDirectoryPath(string(parts[3])) {
		return DirectoryListing{}, errors.New("host returned an invalid folder path")
	}
	listing := DirectoryListing{Home: string(parts[2]), Path: string(parts[3]), Directories: []DirectoryEntry{}, Truncated: len(parts[4]) > directoryOutputLimit}
	if listing.Path != "/" {
		parent := path.Dir(listing.Path)
		listing.Parent = &parent
	}
	names := parts[4]
	if len(names) > 0 {
		// A byte cap can end inside a name or UTF-8 sequence. Never expose that
		// partial entry as a selectable filesystem path.
		last := bytes.LastIndexByte(names, 0)
		if last < len(names)-1 {
			listing.Truncated = true
		}
		if last < 0 {
			names = nil
		} else {
			names = names[:last]
		}
	}
	for _, raw := range bytes.Split(names, []byte{0}) {
		name := strings.TrimPrefix(string(raw), "./")
		if name == "" || name == "." || name == ".." || !q.Matches(name) {
			continue
		}
		entryPath := path.Join(listing.Path, name)
		if !validDirectoryPath(entryPath) {
			continue
		}
		listing.Directories = append(listing.Directories, DirectoryEntry{Name: name, Path: entryPath})
	}
	SortDirectories(&listing)
	return listing, nil
}

func validDirectoryPath(value string) bool {
	return strings.HasPrefix(value, "/") && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func SortDirectories(listing *DirectoryListing) {
	sort.Slice(listing.Directories, func(i, j int) bool { return listing.Directories[i].Name < listing.Directories[j].Name })
	if len(listing.Directories) > DirectoryLimit {
		listing.Truncated = true
		listing.Directories = listing.Directories[:DirectoryLimit]
	}
}
