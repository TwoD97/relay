package sessionimage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

var storeMu sync.Mutex

func Store(ctx context.Context, root, key string, image Image) (string, error) {
	if !ValidKey(key) || !image.Valid() {
		return "", errors.New("invalid image transfer")
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The controller's state root already exists. Validate every app-owned
	// component; do not follow links or relax another user's directory ownership.
	for _, dir := range []string{root, filepath.Join(root, "images"), filepath.Join(root, "images", key)} {
		if err := privateDirectory(dir); err != nil {
			return "", err
		}
	}
	folder := filepath.Join(root, "images", key)
	target := filepath.Join(folder, image.Filename())
	if err := verifyFile(target, image); err == nil {
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return "", err
	}
	if len(entries) >= MaxFiles {
		return "", ErrQuota
	}
	var total int64
	for _, entry := range entries {
		f, err := openOwned(filepath.Join(folder, entry.Name()), false)
		if err != nil {
			return "", err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return "", err
		}
		total += info.Size()
		if total+int64(image.Size()) > SessionQuota {
			return "", ErrQuota
		}
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return "", err
	}
	temporary := filepath.Join(folder, fmt.Sprintf(".image-%x", random))
	file, err := openOwned(temporary, true)
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary)
	_, err = io.Copy(file, image.Reader())
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	// A hard link publishes a complete immutable inode without replacing an
	// existing file. Concurrent identical writes may reuse only verified bytes.
	if err = os.Link(temporary, target); err != nil {
		if verifyErr := verifyFile(target, image); verifyErr != nil {
			return "", errors.Join(err, verifyErr)
		}
	}
	return target, nil
}
func verifyFile(path string, image Image) error {
	file, err := openOwned(path, false)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != int64(image.Size()) {
		return errors.New("existing image does not match its immutable identity")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, MaxBytes+1)); err != nil {
		return err
	}
	if fmt.Sprintf("%x", hash.Sum(nil)) != image.SHA256() {
		return errors.New("existing image checksum does not match; no file was replaced")
	}
	return nil
}
