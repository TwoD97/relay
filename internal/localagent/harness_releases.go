package localagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	terminal "github.com/TwoD97/relay/internal/runtime"
)

var releaseVersion = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}$`)
var releaseDirectory = regexp.MustCompile(`^[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}-[a-f0-9]{32}$`)

const claudeReleaseBase = "https://downloads.claude.ai/claude-code-releases/"
const maxHarnessDownload = 512 << 20

// Install publishes a new private immutable release and then atomically moves
// its pointer. It never opens session metadata or changes provider credentials.
// Already-running binaries remain on disk, including on Windows upgrades.
func Install(ctx context.Context, stateDir, harness, action string, out io.Writer) error {
	return installWithClient(ctx, stateDir, harness, action, out, harnessClient())
}

func installWithClient(ctx context.Context, stateDir, harness, action string, out io.Writer, client *http.Client) error {
	if !validHarness(harness) || (action != "install" && action != "update" && action != "repair") {
		return errors.New("choose claude or codex and install, update, or repair")
	}
	dir, err := filepath.Abs(stateDir)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	s := &Server{stateDir: dir, home: home, executable: exe}
	if out == nil {
		out = io.Discard
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	root := filepath.Join(dir, "tools", "harnesses", harness)
	for _, p := range []string{dir, filepath.Join(dir, "tools"), filepath.Join(dir, "tools", "harnesses"), root, filepath.Join(root, "releases")} {
		if err := privateDir(p); err != nil {
			return err
		}
	}
	lock, err := lockHarnessInstall(filepath.Join(root, "install.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if action == "install" {
		if existing, e := s.discover(harness); e == nil {
			if _, e = s.probe(ctx, existing, "--version"); e == nil {
				fmt.Fprintln(out, "This harness is already installed. Use Update or Repair for a managed replacement.")
				return nil
			}
		}
	}
	staging := filepath.Join(root, "releases", ".preparing-"+id())
	if err := privateDir(staging); err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	var version, relative string
	fmt.Fprintf(out, "Preparing %s %s. Existing authentication and running sessions are preserved.\r\n", harnessNames[harness], action)
	if harness == "claude" {
		version, relative, err = prepareClaude(ctx, client, staging, out)
	} else {
		version, relative, err = prepareCodex(ctx, client, staging, out)
	}
	if err != nil {
		return err
	}
	downloaded := filepath.Join(staging, filepath.FromSlash(relative))
	versionOutput, err := s.probe(ctx, downloaded, "--version")
	if err != nil {
		return errors.New("Downloaded harness could not run; current installation preserved")
	}
	found := false
	for _, field := range strings.Fields(versionOutput) {
		if field == version {
			found = true
		}
	}
	if !found {
		return errors.New("Downloaded harness version did not match its verified release")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	release := version + "-" + id()
	destination := filepath.Join(root, "releases", release)
	if err = os.Rename(staging, destination); err != nil {
		return err
	}
	pointer := harnessPointer{Format: 1, Harness: harness, Release: release, Executable: relative}
	data, _ := json.Marshal(pointer)
	if err = atomicWrite(filepath.Join(root, "current.json"), data); err != nil {
		return errors.New("Harness activation could not be confirmed; previous files were preserved")
	}
	fmt.Fprintf(out, "%s %s is ready. New sessions use this release; existing sessions keep their original files.\r\n", harnessNames[harness], version)
	return nil
}
func harnessClient() *http.Client {
	return &http.Client{Timeout: 8 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 || !trustedHarnessURL(req.URL) || (len(via) > 0 && req.URL.Host != via[0].URL.Host) {
			return errors.New("untrusted harness download redirect")
		}
		return nil
	}}
}
func trustedHarnessURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Host == "downloads.claude.ai" || u.Host == "registry.npmjs.org")
}
func fetchHarness(ctx context.Context, client *http.Client, address string, limit int64, out io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, "GET", address, nil)
	if err != nil || !trustedHarnessURL(req.URL) {
		return errors.New("untrusted harness download URL")
	}
	response, err := client.Do(req)
	if err != nil {
		return errors.New("Could not download the harness release")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Harness download returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return errors.New("harness download exceeds limit")
	}
	n, err := io.Copy(out, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("harness download exceeds limit")
	}
	return nil
}
func claudePlatform() (string, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return "", errors.New("native harnesses require amd64 or arm64")
	}
	if runtime.GOOS == "windows" {
		return "win32-" + arch, nil
	}
	if runtime.GOOS == "linux" {
		platform := "linux-" + arch
		if files, _ := filepath.Glob("/lib/ld-musl-*.so.1"); len(files) > 0 {
			platform += "-musl"
		}
		return platform, nil
	}
	return "", errors.New("unsupported native harness platform")
}
func codexPlatform() (string, string, error) {
	arch, tripleArch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH], map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		return "", "", errors.New("native harnesses require amd64 or arm64")
	}
	if runtime.GOOS == "windows" {
		return "win32-" + arch, tripleArch + "-pc-windows-msvc", nil
	}
	if runtime.GOOS == "linux" {
		return "linux-" + arch, tripleArch + "-unknown-linux-musl", nil
	}
	return "", "", errors.New("unsupported native harness platform")
}
func executableName(id string) string {
	if runtime.GOOS == "windows" {
		return id + ".exe"
	}
	return id
}

func prepareClaude(ctx context.Context, client *http.Client, staging string, out io.Writer) (string, string, error) {
	var latest, manifest, signature bytes.Buffer
	if err := fetchHarness(ctx, client, claudeReleaseBase+"latest", 128, &latest); err != nil {
		return "", "", err
	}
	version := strings.TrimSpace(latest.String())
	if !releaseVersion.MatchString(version) {
		return "", "", errors.New("invalid Claude version")
	}
	base := claudeReleaseBase + version + "/"
	if err := fetchHarness(ctx, client, base+"manifest.json", 128<<10, &manifest); err != nil {
		return "", "", err
	}
	if err := fetchHarness(ctx, client, base+"manifest.json.sig", 64<<10, &signature); err != nil {
		return "", "", err
	}
	if err := terminal.VerifyClaudeRelease(manifest.Bytes(), signature.Bytes()); err != nil {
		return "", "", err
	}
	var metadata struct {
		Version   string `json:"version"`
		Platforms map[string]struct {
			Binary   string `json:"binary"`
			Checksum string `json:"checksum"`
			Size     int64  `json:"size"`
		} `json:"platforms"`
	}
	if json.Unmarshal(manifest.Bytes(), &metadata) != nil || metadata.Version != version {
		return "", "", errors.New("unexpected Claude signed manifest version")
	}
	platform, err := claudePlatform()
	if err != nil {
		return "", "", err
	}
	entry, ok := metadata.Platforms[platform]
	expected, err := hex.DecodeString(entry.Checksum)
	if !ok || err != nil || len(expected) != sha256.Size || entry.Size <= 0 || entry.Size > maxHarnessDownload || entry.Binary != executableName("claude") {
		return "", "", errors.New("Claude signed manifest does not contain this platform")
	}
	relative := executableName("claude")
	file, err := openPrivate(filepath.Join(staging, relative), true)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(out, "Downloading Claude Code %s for %s; vendor signature verified.\r\n", version, platform)
	hash := sha256.New()
	counter := &harnessProgress{out: out, total: entry.Size}
	err = fetchHarness(ctx, client, base+platform+"/"+entry.Binary, entry.Size, io.MultiWriter(file, hash, counter))
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return "", "", err
	}
	if counter.n != entry.Size || subtle.ConstantTimeCompare(expected, hash.Sum(nil)) != 1 {
		return "", "", errors.New("Claude binary checksum or size mismatch")
	}
	if err := os.Chmod(filepath.Join(staging, relative), 0700); err != nil {
		return "", "", err
	}
	return version, relative, nil
}

type codexMetadata struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	OS                   []string          `json:"os"`
	CPU                  []string          `json:"cpu"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	Dist                 struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
	} `json:"dist"`
}

func prepareCodex(ctx context.Context, client *http.Client, staging string, out io.Writer) (string, string, error) {
	var latest bytes.Buffer
	if err := fetchHarness(ctx, client, "https://registry.npmjs.org/@openai/codex/latest", 128<<10, &latest); err != nil {
		return "", "", err
	}
	var main codexMetadata
	if json.Unmarshal(latest.Bytes(), &main) != nil || main.Name != "@openai/codex" || !releaseVersion.MatchString(main.Version) {
		return "", "", errors.New("invalid official Codex metadata")
	}
	platform, triple, err := codexPlatform()
	if err != nil {
		return "", "", err
	}
	platformVersion := main.Version + "-" + platform
	if main.OptionalDependencies["@openai/codex-"+platform] != "npm:@openai/codex@"+platformVersion {
		return "", "", errors.New("Codex platform package is not the expected official dependency")
	}
	var raw bytes.Buffer
	if err = fetchHarness(ctx, client, "https://registry.npmjs.org/@openai/codex/"+platformVersion, 128<<10, &raw); err != nil {
		return "", "", err
	}
	var meta codexMetadata
	if json.Unmarshal(raw.Bytes(), &meta) != nil || meta.Name != "@openai/codex" || meta.Version != platformVersion {
		return "", "", errors.New("invalid Codex platform metadata")
	}
	address := "https://registry.npmjs.org/@openai/codex/-/codex-" + platformVersion + ".tgz"
	expected, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(meta.Dist.Integrity, "sha512-"))
	if err != nil || !strings.HasPrefix(meta.Dist.Integrity, "sha512-") || len(expected) != sha512.Size || meta.Dist.Tarball != address {
		return "", "", errors.New("Codex platform archive integrity is invalid")
	}
	archive, err := openPrivate(filepath.Join(staging, ".codex-archive-"+id()), true)
	if err != nil {
		return "", "", err
	}
	defer os.Remove(archive.Name())
	hash := sha512.New()
	fmt.Fprintf(out, "Downloading official Codex %s for %s; verifying SHA-512.\r\n", main.Version, platform)
	err = fetchHarness(ctx, client, address, maxHarnessDownload, io.MultiWriter(archive, hash, &harnessProgress{out: out}))
	err = errors.Join(err, archive.Sync(), archive.Close())
	if err == nil && subtle.ConstantTimeCompare(expected, hash.Sum(nil)) != 1 {
		err = errors.New("Codex archive checksum mismatch")
	}
	if err != nil {
		return "", "", err
	}
	reader, err := openPrivate(archive.Name(), false)
	if err != nil {
		return "", "", err
	}
	err = extractCodex(reader, staging)
	err = errors.Join(err, reader.Close())
	if err != nil {
		return "", "", err
	}
	for _, folder := range []string{"bin", "codex"} {
		relative := "package/vendor/" + triple + "/" + folder + "/" + executableName("codex")
		if executableFile(filepath.Join(staging, filepath.FromSlash(relative))) {
			return main.Version, relative, nil
		}
	}
	return "", "", errors.New("verified Codex archive has no expected native executable")
}
func safeArchivePath(name string) bool {
	if name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\\:\x00\r\n") || name == ".." || strings.HasPrefix(name, "../") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.TrimRight(part, ". ") != part {
			return false
		}
		base := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
			return false
		}
	}
	return true
}
func extractCodex(source io.Reader, dest string) error {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	// Bound decompressed framing/PAX data too, not just regular file sizes.
	expanded := &io.LimitedReader{R: gzipReader, N: (1 << 30) + 1}
	reader := tar.NewReader(expanded)
	var total int64
	count := 0
	for {
		header, err := reader.Next()
		if expanded.N <= 0 {
			return errors.New("Codex expanded archive exceeds limit")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		count++
		if count > 5000 {
			return errors.New("Codex archive contains too many entries")
		}
		name := strings.TrimSuffix(header.Name, "/")
		if !safeArchivePath(name) || (name != "package" && !strings.HasPrefix(name, "package/")) {
			return errors.New("unsafe Codex archive path")
		}
		destination := filepath.Join(dest, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err = privateDir(destination); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			total += header.Size
			if header.Size < 0 || total > 1<<30 {
				return errors.New("Codex expanded archive exceeds limit")
			}
			if err = privateDir(filepath.Dir(destination)); err != nil {
				return err
			}
			file, err := openPrivate(destination, true)
			if err != nil {
				return err
			}
			if err := os.Chmod(destination, 0700); err != nil {
				return err
			}
			_, err = io.CopyN(file, reader, header.Size)
			err = errors.Join(err, file.Close())
			if err != nil {
				return err
			}
		default:
			return errors.New("Codex archive links and special files are not allowed")
		}
	}
}

type harnessProgress struct {
	out            io.Writer
	n, last, total int64
}

func (p *harnessProgress) Write(data []byte) (int, error) {
	p.n += int64(len(data))
	if p.n-p.last >= 32<<20 || p.total > 0 && p.n == p.total {
		fmt.Fprintf(p.out, "Downloaded %d MiB.\r\n", p.n>>20)
		p.last = p.n
	}
	return len(data), nil
}
