package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const stagedFileLimit = 256 << 20

func stageHashFile(path string) (string, error) {
	file, err := stageReadFile(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() <= 0 || info.Size() > stagedFileLimit {
		return "", errors.New("controller release file must be nonempty and under 256 MiB")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, stagedFileLimit+1))
	if err != nil {
		return "", err
	}
	if n != info.Size() {
		return "", errors.New("controller release file changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func stageManifest(directory string) ([]byte, map[string]string, error) {
	file, err := stageReadFile(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 513))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > 512 {
		return nil, nil, errors.New("runtime checksum manifest exceeds 512 bytes")
	}
	digests := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || (fields[1] != "relay-linux-amd64" && fields[1] != "relay-linux-arm64") {
			return nil, nil, errors.New("runtime manifest must contain exactly the two Linux architectures")
		}
		digest, err := hex.DecodeString(fields[0])
		if err != nil || len(digest) != sha256.Size || digests[fields[1]] != "" {
			return nil, nil, errors.New("invalid or duplicate runtime checksum")
		}
		digests[fields[1]] = strings.ToLower(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}
	if len(digests) != 2 {
		return nil, nil, errors.New("runtime manifest must verify both Linux architectures")
	}
	return data, digests, nil
}

func stageCopyFile(source, destination, expected string) error {
	input, err := stageReadFile(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := stageCreateFile(destination)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, stagedFileLimit+1))
	if copyErr == nil && (n <= 0 || n > stagedFileLimit || hex.EncodeToString(hash.Sum(nil)) != expected) {
		copyErr = errors.New("controller release bytes changed while copying")
	}
	if copyErr == nil && filepath.Base(destination) == stagedControllerFilename {
		copyErr = output.Chmod(0700)
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func stageWriteManifest(path string, data []byte) error {
	file, err := stageCreateFile(path)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// Keep the controller and remote payloads in a verified private release. The
// installer can replace its resources without changing a running controller.
func stageController(opts uiOptions) (uiOptions, error) {
	executableHash, err := stageHashFile(opts.Executable)
	if err != nil {
		return opts, fmt.Errorf("verify controller executable: %w", err)
	}
	manifest, digests, err := stageManifest(opts.BinaryDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return opts, err
	}
	if errors.Is(err, os.ErrNotExist) {
		manifest = nil
		digests = map[string]string{}
	}
	for name, expected := range digests {
		actual, err := stageHashFile(filepath.Join(opts.BinaryDir, name))
		if err != nil {
			return opts, err
		}
		if actual != expected {
			return opts, fmt.Errorf("runtime checksum mismatch for %s", name)
		}
	}
	manifestHash := sha256.Sum256(manifest)
	releaseHash := sha256.Sum256([]byte(executableHash + ":" + hex.EncodeToString(manifestHash[:])))
	parent := filepath.Join(opts.StateDir, "controller-releases")
	if err := privateControllerDirectory(parent); err != nil {
		return opts, err
	}
	release := filepath.Join(parent, hex.EncodeToString(releaseHash[:]))
	verify := func() error {
		if err := privateControllerDirectory(release); err != nil {
			return err
		}
		actual, err := stageHashFile(filepath.Join(release, stagedControllerFilename))
		if err != nil {
			return err
		}
		if actual != executableHash {
			return errors.New("existing private controller release has different executable bytes")
		}
		if manifest != nil {
			installed, _, err := stageManifest(release)
			if err != nil {
				return err
			}
			if !bytes.Equal(installed, manifest) {
				return errors.New("existing controller release has a different runtime manifest")
			}
		}
		for name, expected := range digests {
			actual, err := stageHashFile(filepath.Join(release, name))
			if err != nil {
				return err
			}
			if actual != expected {
				return fmt.Errorf("existing private runtime checksum mismatch for %s", name)
			}
		}
		return nil
	}
	if _, err := os.Lstat(release); err == nil {
		if err := verify(); err != nil {
			return opts, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return opts, err
	} else {
		staging, err := privateControllerTempDirectory(parent)
		if err != nil {
			return opts, err
		}
		defer os.RemoveAll(staging)
		if err := privateControllerDirectory(staging); err != nil {
			return opts, err
		}
		if err := stageCopyFile(opts.Executable, filepath.Join(staging, stagedControllerFilename), executableHash); err != nil {
			return opts, err
		}
		for name, expected := range digests {
			if err := stageCopyFile(filepath.Join(opts.BinaryDir, name), filepath.Join(staging, name), expected); err != nil {
				return opts, err
			}
		}
		if manifest != nil {
			if err := stageWriteManifest(filepath.Join(staging, "SHA256SUMS"), manifest); err != nil {
				return opts, err
			}
		}
		if err := os.Rename(staging, release); err != nil {
			if _, existsErr := os.Lstat(release); existsErr != nil {
				return opts, fmt.Errorf("activate private controller release: %w", err)
			}
		}
		if err := verify(); err != nil {
			return opts, err
		}
	}
	opts.Executable = filepath.Join(release, stagedControllerFilename)
	opts.BinaryDir = release
	return opts, nil
}
