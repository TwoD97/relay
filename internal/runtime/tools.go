//go:build linux

package runtime

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Digests pinned from https://nodejs.org/dist/v24.21.0/SHASUMS256.txt.
// Node's official Linux binaries require glibc >= 2.28; musl is not supported.
const nodeVersion = "v24.21.0"

var nodeDigests = map[string]string{
	"amd64": "6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff",
	"arm64": "724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5",
}

func nodeArchiveRoot() string {
	arch := goruntime.GOARCH
	if arch == "amd64" {
		arch = "x64"
	}
	return "node-" + nodeVersion + "-linux-" + arch
}
func (s *Server) ensureNPM(ctx context.Context, out io.Writer) (string, error) {
	select {
	case s.toolsMu <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-s.toolsMu }()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest, ok := nodeDigests[goruntime.GOARCH]
	if !ok || goruntime.GOOS != "linux" {
		return "", errors.New("automatic Node installation supports Linux amd64 and arm64")
	}
	toolsDir := filepath.Join(s.stateDir, "tools")
	if err := privateDir(toolsDir); err != nil {
		return "", err
	}
	lock, err := lockNodePreparation(ctx, filepath.Join(toolsDir, ".node.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	destination := filepath.Join(toolsDir, nodeArchiveRoot())
	info, err := os.Lstat(destination)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	existing := err == nil
	if existing && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return "", errors.New("private Node path is not a regular directory; existing files were preserved")
	}
	if node, findErr := s.findCommand("node"); findErr == nil {
		if version, probeErr := s.probe(ctx, node, "--version"); probeErr == nil && nodeMajor(version) >= 22 {
			if npm, findErr := s.findCommand("npm"); findErr == nil {
				if version, probeErr := s.probe(ctx, npm, "--version"); probeErr == nil && releaseVersion.MatchString(strings.TrimSpace(version)) {
					return npm, nil
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	npm := filepath.Join(destination, "bin", "npm")
	fmt.Fprintf(out, "Preparing private Node.js %s (verified SHA-256)...\r\n", nodeVersion)
	temporary, err := os.CreateTemp(toolsDir, ".node-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	defer temporary.Close()
	url := "https://nodejs.org/dist/" + nodeVersion + "/" + nodeArchiveRoot() + ".tar.gz"
	if err = downloadVerified(ctx, url, digest, temporary); err != nil {
		return "", err
	}
	if _, err = temporary.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(toolsDir, ".node-prepare-*")
	if err != nil {
		return "", err
	}
	keepPrevious := false
	defer func() {
		if !keepPrevious {
			_ = os.RemoveAll(staging)
		}
	}()
	if err = extractNode(temporary, staging, nodeArchiveRoot()); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	prepared := filepath.Join(staging, nodeArchiveRoot())
	if _, err = s.verifyPrivateNode(ctx, prepared, filepath.Join(prepared, "bin", "npm")); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if existing {
		current, statErr := os.Lstat(destination)
		if statErr != nil || !os.SameFile(info, current) {
			return "", errors.New("private Node directory changed during preparation; existing files preserved")
		}
		// Preserve the old tree and publish without a missing-path interval.
		// In-place replacement or deletion can break a still-running Node job.
		if err = unix.Renameat2(unix.AT_FDCWD, prepared, unix.AT_FDCWD, destination, unix.RENAME_EXCHANGE); err != nil {
			return "", fmt.Errorf("cannot atomically repair private Node; existing files preserved: %w", err)
		}
		keepPrevious = true
		fmt.Fprintf(out, "Previous private Node files retained at %s.\r\n", prepared)
	} else if err = unix.Renameat2(unix.AT_FDCWD, prepared, unix.AT_FDCWD, destination, unix.RENAME_NOREPLACE); err != nil {
		return "", err
	}
	dir, err := os.Open(toolsDir)
	if err != nil {
		return "", err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return "", err
	}
	fmt.Fprintln(out, "Node.js ready.\r")
	return npm, nil
}

// Different standalone harness workers can share one private Node prefix.
// Waiting for publication is bounded by the caller's operation context.
func lockNodePreparation(ctx context.Context, path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("open private Node preparation lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("private Node preparation lock is not a regular file")
	}
	for {
		if err = ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func downloadVerified(ctx context.Context, url, digest string, dest io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || req.URL.Scheme != "https" || req.URL.Host != "nodejs.org" {
			return errors.New("untrusted download redirect")
		}
		return nil
	}}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download Node: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("download Node: HTTP %d", response.StatusCode)
	}
	const maximum = 100 << 20
	if response.ContentLength > maximum {
		return errors.New("Node archive exceeds size limit")
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(dest, hash), io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return err
	}
	if n > maximum {
		return errors.New("Node archive exceeds size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("Node archive checksum mismatch; installation aborted")
	}
	return nil
}
func extractNode(source io.Reader, dest, root string) error {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	reader := tar.NewReader(io.LimitReader(gzipReader, 512<<20))
	var total int64
	var files int
	type link struct{ name, target string }
	var links []link
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		files++
		if files > 30000 {
			return errors.New("too many Node archive entries")
		}
		name := path.Clean(header.Name)
		if name != root && !strings.HasPrefix(name, root+"/") {
			return errors.New("archive entry outside expected root")
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > 512<<20 {
				return errors.New("unpacked Node archive exceeds size limit")
			}
			if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			mode := os.FileMode(0600)
			if header.Mode&0111 != 0 {
				mode = 0700
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(f, reader, header.Size)
			closeErr := f.Close()
			if err = errors.Join(copyErr, closeErr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			resolved := path.Clean(path.Join(path.Dir(name), header.Linkname))
			if path.IsAbs(header.Linkname) || (resolved != root && !strings.HasPrefix(resolved, root+"/")) {
				return errors.New("archive symlink outside expected root")
			}
			links = append(links, link{target, header.Linkname})
		default:
			return errors.New("unsupported Node archive entry type")
		}
	}
	for _, link := range links {
		if err = noSymlinkParents(dest, filepath.Dir(link.name)); err != nil {
			return err
		}
		if err = os.MkdirAll(filepath.Dir(link.name), 0700); err != nil {
			return err
		}
		if err = os.Symlink(link.target, link.name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) verifyPrivateNode(ctx context.Context, destination, npm string) (string, error) {
	node := filepath.Join(destination, "bin", "node")
	if version, err := s.probe(ctx, node, "--version"); err != nil || strings.TrimSpace(version) != nodeVersion {
		return "", errors.New("private Node.js could not run; official Linux binaries require glibc 2.28 or newer. On a musl host, install a compatible Node.js 22+ and npm with the host package manager, then retry")
	}
	// npm's env-node shebang would select the damaged current prefix while
	// checking a replacement. Execute it with its matching staged Node.
	if version, err := s.probe(ctx, node, npm, "--version"); err != nil || !releaseVersion.MatchString(strings.TrimSpace(version)) {
		return "", errors.New("private npm could not run; the previous installation was preserved")
	}
	return npm, nil
}

func noSymlinkParents(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	current := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("archive symlink parent is not a directory")
		}
	}
	return nil
}
