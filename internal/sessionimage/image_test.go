package sessionimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func synthetic(t *testing.T, format string) []byte {
	t.Helper()
	picture := image.NewNRGBA(image.Rect(0, 0, 13, 9))
	picture.Set(3, 4, color.NRGBA{R: 255, G: 40, B: 80, A: 255})
	var encoded bytes.Buffer
	var err error
	if format == "jpeg" {
		err = jpeg.Encode(&encoded, picture, nil)
	} else {
		err = png.Encode(&encoded, picture)
	}
	if err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}
func TestValidateImages(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		data := synthetic(t, format)
		image, err := Validate(data, "image/"+format)
		if err != nil || image.Size() != len(data) || !image.Valid() {
			t.Fatalf("valid %s: %v", format, err)
		}
		data[0] ^= 255
		actual, _ := io.ReadAll(image.Reader())
		if actual[0] == data[0] {
			t.Fatal("caller changed immutable image bytes")
		}
	}
	for _, test := range []struct {
		data []byte
		mime string
	}{{[]byte("#!/bin/sh\necho bad"), "image/png"}, {synthetic(t, "png"), "image/jpeg"}, {synthetic(t, "png")[:35], "image/png"}, {synthetic(t, "png"), "text/plain"}, {make([]byte, MaxBytes+1), "image/png"}} {
		if _, err := Validate(test.data, test.mime); err == nil {
			t.Fatalf("invalid image accepted: %s %d", test.mime, len(test.data))
		}
	}
}
func TestImageDimensionsCheckedBeforeDecode(t *testing.T) {
	data := synthetic(t, "png")
	binary.BigEndian.PutUint32(data[16:20], 32768)
	binary.BigEndian.PutUint32(data[20:24], 32768)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	if _, err := Validate(data, "image/png"); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Fatalf("oversized header not rejected before decode: %v", err)
	}
}

func TestImageStoreImmutablePrivateQuotaAndCancellation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	key, _ := Key(strings.Repeat("a", 32), time.Unix(100, 1))
	image, err := Validate(synthetic(t, "png"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	results := make(chan string, 8)
	failures := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			path, err := Store(context.Background(), root, key, image)
			if err != nil {
				failures <- err
			} else {
				results <- path
			}
		}()
	}
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	target := ""
	for path := range results {
		if target != "" && target != path {
			t.Fatal("duplicate image paths")
		}
		target = path
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, synthetic(t, "png")) {
		t.Fatal("stored bytes changed", err)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary images leaked", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = Store(ctx, root, key, image); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled image stored", err)
	}
	if err = os.WriteFile(target, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Store(context.Background(), root, key, image); err == nil {
		t.Fatal("corrupt immutable file replaced")
	}
	if err = os.Remove(target); err != nil {
		t.Fatal(err)
	}
	quota, err := openOwned(filepath.Join(filepath.Dir(target), "existing.png"), true)
	if err != nil {
		t.Fatal(err)
	}
	if err = quota.Truncate(SessionQuota); err != nil {
		t.Fatal(err)
	}
	quota.Close()
	if _, err = Store(context.Background(), root, key, image); !errors.Is(err, ErrQuota) {
		t.Fatal("quota not enforced", err)
	}
}
func TestImageStoreRejectsSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := privateDirectory(root); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "images")); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	key, _ := Key(strings.Repeat("b", 32), time.Unix(100, 0))
	image, _ := Validate(synthetic(t, "png"), "image/png")
	if _, err := Store(context.Background(), root, key, image); err == nil {
		t.Fatal("followed image directory symlink")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatal("wrote outside image storage")
	}
}
