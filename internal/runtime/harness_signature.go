package runtime

import (
	"bytes"
	"crypto"
	_ "embed"
	"encoding/hex"
	"errors"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/crypto/openpgp/packet"
	"io"
	"strings"
)

// Anthropic's documented release key, retrieved from the official fixed URL.
// https://code.claude.com/docs/en/setup#verify-the-manifest-signature
// The fingerprint is also checked before every detached signature verification.
//
//go:embed keys/claude-release.asc
var claudeReleaseKey []byte

const claudeReleaseFingerprint = "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"

func verifyClaudeManifest(manifest, signature, key []byte, fingerprint string) error {
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil || len(keys) != 1 || strings.ToUpper(hex.EncodeToString(keys[0].PrimaryKey.Fingerprint[:])) != fingerprint {
		return errors.New("Claude release signing key fingerprint mismatch")
	}
	if bytes.HasPrefix(signature, []byte("-----BEGIN")) {
		block, decodeErr := armor.Decode(bytes.NewReader(signature))
		if decodeErr != nil || block.Type != "PGP SIGNATURE" {
			return errors.New("invalid Claude manifest signature")
		}
		signature, err = io.ReadAll(io.LimitReader(block.Body, 64<<10))
		if err != nil {
			return err
		}
	}
	// This legacy package is used only for this pinned RSA public key and a
	// SHA-2 detached signature; no encryption, key discovery, or key import.
	parsed, err := packet.Read(bytes.NewReader(signature))
	if err != nil {
		return errors.New("invalid Claude manifest signature")
	}
	sig, ok := parsed.(*packet.Signature)
	if !ok || (sig.Hash != crypto.SHA256 && sig.Hash != crypto.SHA384 && sig.Hash != crypto.SHA512) {
		return errors.New("Claude manifest requires a SHA-2 signature")
	}
	if _, err = openpgp.CheckDetachedSignature(keys, bytes.NewReader(manifest), bytes.NewReader(signature)); err != nil {
		return errors.New("Claude manifest signature verification failed; installation aborted")
	}
	return nil
}

// VerifyClaudeRelease validates the vendor's detached SHA-2 manifest signature
// against the same pinned release key on every supported desktop platform.
func VerifyClaudeRelease(manifest, signature []byte) error {
	return verifyClaudeManifest(manifest, signature, claudeReleaseKey, claudeReleaseFingerprint)
}
