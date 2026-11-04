package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
)

const whatsappManifestName = "SHA256SUMS.whatsapp"

var optionalWhatsAppArtifacts = map[string]bool{
	"relay-whatsapp-personal-linux-amd64":       true,
	"relay-whatsapp-personal-linux-arm64":       true,
	"relay-whatsapp-personal-windows-amd64.exe": true,
}

// The independent connector is optional. If supplied, every packaged target
// must be verified and copied into the immutable release with the controller.
func stageWhatsAppManifest(directory string) ([]byte, map[string]string, error) {
	file, err := stageReadFile(filepath.Join(directory, whatsappManifestName))
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 1024 {
		return nil, nil, errors.New("optional connector checksum manifest is too large")
	}
	digests := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !optionalWhatsAppArtifacts[fields[1]] || digests[fields[1]] != "" {
			return nil, nil, errors.New("optional connector manifest must contain exactly its three supported targets")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size {
			return nil, nil, errors.New("invalid optional connector checksum")
		}
		digests[fields[1]] = strings.ToLower(fields[0])
	}
	if scanner.Err() != nil || len(digests) != len(optionalWhatsAppArtifacts) {
		return nil, nil, errors.New("optional connector manifest must verify all supported targets")
	}
	return data, digests, nil
}
