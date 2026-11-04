package personalwhatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const helperLimit = 128 << 20

var helperNames = []string{"relay-whatsapp-personal-linux-amd64", "relay-whatsapp-personal-linux-arm64", "relay-whatsapp-personal-windows-amd64.exe"}

func platformHelper() string {
	for _, name := range helperNames {
		suffix := runtime.GOOS + "-" + runtime.GOARCH
		if runtime.GOOS == "windows" {
			suffix += ".exe"
		}
		if strings.HasSuffix(name, suffix) {
			return name
		}
	}
	return ""
}
func helperManifest(data []byte) (map[string]string, error) {
	result := map[string]string{}
	if len(data) > 4096 {
		return nil, errors.New("Optional WhatsApp connector manifest is too large")
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, errors.New("Optional WhatsApp connector manifest is invalid")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != 32 || fields[0] != strings.ToLower(fields[0]) {
			return nil, errors.New("Optional WhatsApp connector digest is invalid")
		}
		valid := false
		for _, name := range helperNames {
			valid = valid || name == fields[1]
		}
		if !valid || result[fields[1]] != "" {
			return nil, errors.New("Optional WhatsApp connector manifest names are invalid")
		}
		result[fields[1]] = fields[0]
	}
	if len(result) != len(helperNames) {
		return nil, errors.New("Optional WhatsApp connector manifest is incomplete")
	}
	return result, nil
}
func stageHelper(ctx context.Context, bundle, parent string) (string, error) {
	name := platformHelper()
	if name == "" {
		return "", errors.New("Optional WhatsApp connector does not support this platform")
	}
	manifest, err := openRegular(filepath.Join(bundle, "SHA256SUMS.whatsapp"), 4096)
	if err != nil {
		return "", errors.New("Optional personal WhatsApp connector is not installed; install a Relay package containing it")
	}
	data, err := io.ReadAll(io.LimitReader(manifest, 4097))
	manifest.Close()
	if err != nil {
		return "", errors.New("Optional WhatsApp connector manifest could not be read")
	}
	hashes, err := helperManifest(data)
	if err != nil {
		return "", err
	}
	digest := hashes[name]
	if err = privateDirectory(parent); err != nil {
		return "", err
	}
	release := filepath.Join(parent, digest)
	if err = privateDirectory(release); err != nil {
		return "", err
	}
	final := filepath.Join(release, name)
	if _, err = os.Lstat(final); err == nil {
		if err = verifyHelper(ctx, final, digest); err != nil {
			return "", err
		}
		return final, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	source, err := openRegular(filepath.Join(bundle, name), helperLimit)
	if err != nil {
		return "", errors.New("Optional WhatsApp connector binary is missing or unsafe")
	}
	defer source.Close()
	staged, err := os.CreateTemp(release, ".stage-")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(staged, hash), &contextReader{ctx: ctx, reader: io.LimitReader(source, helperLimit+1)})
	if err == nil && hex.EncodeToString(hash.Sum(nil)) != digest {
		err = errors.New("Optional WhatsApp connector checksum does not match")
	}
	if err == nil {
		err = staged.Chmod(0700)
	}
	if err == nil {
		err = staged.Sync()
	}
	closeErr := staged.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	// Link publishes without replacing an existing or currently running executable.
	if err = os.Link(staged.Name(), final); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err = verifyHelper(ctx, final, digest); err != nil {
		return "", err
	}
	return final, nil
}
func verifyHelper(ctx context.Context, path, want string) error {
	f, err := openRegular(path, helperLimit)
	if err != nil {
		return errors.New("Staged WhatsApp connector is not a regular file")
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, reader: io.LimitReader(f, helperLimit+1)})
	if err != nil {
		return err
	}
	if n > helperLimit || hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("Staged WhatsApp connector checksum does not match; existing file was preserved")
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
