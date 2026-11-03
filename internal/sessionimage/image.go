// Package sessionimage validates image-only transfers and stores immutable,
// private files. It never sends terminal input or executes provider commands.
package sessionimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"strings"
	"time"
)

const MaxBytes = 10 << 20
const MaxPixels = 40_000_000
const SessionQuota = 100 << 20
const MaxFiles = 128

var ErrQuota = errors.New("this session's image storage is full (100 MiB or 128 files); existing images were preserved")

type Image struct {
	data                    []byte
	mime, extension, digest string
}

func (i Image) Reader() io.Reader { return bytes.NewReader(i.data) }
func (i Image) Size() int         { return len(i.data) }
func (i Image) MIME() string      { return i.mime }
func (i Image) SHA256() string    { return i.digest }
func (i Image) Filename() string  { return i.digest + i.extension }
func (i Image) Valid() bool {
	return len(i.data) > 0 && len(i.data) <= MaxBytes && len(i.digest) == 64 && (i.extension == ".png" || i.extension == ".jpg")
}
func Validate(data []byte, contentType string) (Image, error) {
	media, _, err := mime.ParseMediaType(contentType)
	if err != nil || media != "image/png" && media != "image/jpeg" {
		return Image{}, errors.New("paste a PNG or JPEG image")
	}
	if len(data) == 0 || len(data) > MaxBytes {
		return Image{}, errors.New("image must be between 1 byte and 10 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, errors.New("image data is not a valid PNG or JPEG")
	}
	actual, extension := "image/png", ".png"
	if format == "jpeg" {
		actual, extension = "image/jpeg", ".jpg"
	} else if format != "png" {
		return Image{}, errors.New("unsupported image format")
	}
	if actual != media {
		return Image{}, errors.New("image format does not match its content type")
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > 32768 || config.Height > 32768 || int64(config.Width)*int64(config.Height) > MaxPixels {
		return Image{}, errors.New("image dimensions exceed 40 megapixels or 32768 pixels per side")
	}
	// Decode the complete image, not merely a forged signature/header. Bounds
	// above limit decompression memory before any image-sized allocation happens.
	decoded, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format || decoded.Bounds().Dx() != config.Width || decoded.Bounds().Dy() != config.Height {
		return Image{}, errors.New("image data is incomplete or corrupt")
	}
	digest := sha256.Sum256(data)
	return Image{data: bytes.Clone(data), mime: actual, extension: extension, digest: hex.EncodeToString(digest[:])}, nil
}
func Key(sessionID string, createdAt time.Time) (string, error) {
	id, err := hex.DecodeString(sessionID)
	if err != nil || len(id) != 16 || sessionID != strings.ToLower(sessionID) || createdAt.IsZero() {
		return "", errors.New("invalid session image identity")
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(sessionID+"\x00"+createdAt.UTC().Format(time.RFC3339Nano)))), nil
}
func ValidKey(key string) bool {
	value, err := hex.DecodeString(key)
	return err == nil && len(value) == 32 && hex.EncodeToString(value) == key
}
