package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/TwoD97/relay/internal/sessionimage"
)

var ErrImageTools = errors.New("image transfer requires sha256sum, flock, stat and head on the Linux host; no upload or input was attempted")

// UploadSessionImage uses only the existing authenticated SSH connection. The
// remote daemon and provider process are not modified or restarted.
func (c *Connection) UploadSessionImage(ctx context.Context, key string, image sessionimage.Image) (string, error) {
	if !sessionimage.ValidKey(key) || !image.Valid() {
		return "", errors.New("invalid image transfer")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	output, err := c.Run(ctx, imageUploadScript(key, image), image.Reader())
	if err != nil {
		return "", fmt.Errorf("image upload outcome could not be confirmed; no input was sent and the upload was not retried: %w", err)
	}
	fields := bytes.Split(output, []byte{0})
	if len(fields) != 4 || string(fields[0]) != "relay-image-v1" || len(fields[3]) != 0 {
		return "", errors.New("host returned an invalid image transfer response; no input was sent")
	}
	if string(fields[1]) == "unavailable" {
		return "", ErrImageTools
	}
	if string(fields[1]) == "quota" {
		return "", sessionimage.ErrQuota
	}
	if string(fields[1]) != "ok" || !validDirectoryPath(string(fields[2])) {
		return "", errors.New("host could not safely store the image; no input was sent")
	}
	return string(fields[2]), nil
}
func imageUploadScript(key string, image sessionimage.Image) string {
	return `set -eu
umask 077
fail() { printf 'relay-image-v1\000%s\000\000' "$1"; exit 0; }
command -v sha256sum >/dev/null 2>&1 && command -v flock >/dev/null 2>&1 && command -v stat >/dev/null 2>&1 && command -v head >/dev/null 2>&1 || fail unavailable
private_dir() {
 [ ! -L "$1" ] || fail unsafe
 if [ ! -e "$1" ]; then mkdir -m 700 -- "$1" 2>/dev/null || [ -d "$1" ] || fail unsafe; fi
 [ -d "$1" ] && [ ! -L "$1" ] && [ -O "$1" ] || fail unsafe
}
private_dir "$HOME"
private_dir "$HOME/.local"
private_dir "$HOME/.local/share"
private_dir "$HOME/.local/share/relay"
private_dir "$HOME/.local/share/relay/images"
folder="$HOME/.local/share/relay/images/` + key + `"
private_dir "$folder"
chmod 700 -- "$HOME/.local/share/relay/images" "$folder"
lock="$folder/.lock"
[ ! -L "$lock" ] || fail unsafe
if [ -e "$lock" ]; then [ -f "$lock" ] && [ -O "$lock" ] || fail unsafe; fi
exec 9>>"$lock"
chmod 600 -- "$lock"
flock -w 10 9 || fail busy
temporary=$(mktemp "$folder/.upload.XXXXXXXX")
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM
head -c 10485761 > "$temporary"
[ "$(stat -c %s -- "$temporary")" = '` + fmt.Sprint(image.Size()) + `' ] || fail invalid
[ "$(sha256sum -- "$temporary" | cut -d ' ' -f 1)" = '` + image.SHA256() + `' ] || fail invalid
target="$folder/` + image.Filename() + `"
if [ -e "$target" ] || [ -L "$target" ]; then
 [ ! -L "$target" ] && [ -f "$target" ] && [ -O "$target" ] || fail unsafe
 [ "$(stat -c %s -- "$target")" = '` + fmt.Sprint(image.Size()) + `' ] || fail unsafe
 [ "$(sha256sum -- "$target" | cut -d ' ' -f 1)" = '` + image.SHA256() + `' ] || fail unsafe
else
 total=0; count=0
 for item in "$folder"/* "$folder"/.[!.]* "$folder"/..?*; do
  if [ ! -e "$item" ] && [ ! -L "$item" ]; then continue; fi
  [ "$item" != "$lock" ] || continue
  [ ! -L "$item" ] && [ -f "$item" ] && [ -O "$item" ] || fail unsafe
  size=$(stat -c %s -- "$item")
  total=$((total + size)); count=$((count + 1))
  [ "$total" -le 104857600 ] && [ "$count" -le 128 ] || fail quota
 done
 chmod 600 -- "$temporary"
 ln -- "$temporary" "$target" || fail unsafe
fi
chmod 600 -- "$target"
printf 'relay-image-v1\000ok\000%s\000' "$target"
`
}
