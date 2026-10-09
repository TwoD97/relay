//go:build windows || relay_ssh_native

package transport

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type nativeConfig struct {
	host, user, home, hostKeyAlias, agentPath string
	port                                      int
	identities, knownFiles                    []string
	knownWrite                                string
	identitiesOnly                            bool
}

func (cfg nativeConfig) address() string { return net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port)) }
func (cfg nativeConfig) keyAddress() string {
	if cfg.hostKeyAlias != "" {
		return net.JoinHostPort(cfg.hostKeyAlias, strconv.Itoa(cfg.port))
	}
	return cfg.address()
}

func resolveNativeConfig(ctx context.Context, cfg Config) (nativeConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nativeConfig{}, err
	}
	r := nativeConfig{home: home, host: cfg.Target, port: 22, knownWrite: filepath.Join(home, ".ssh", "known_hosts")}
	if current, err := user.Current(); err == nil {
		r.user = current.Username
		if _, short, ok := strings.Cut(r.user, `\`); ok {
			r.user = short
		}
	}
	if name, host, ok := strings.Cut(cfg.Target, "@"); ok {
		r.user = name
		r.host = host
	}
	r.host = strings.TrimSuffix(strings.TrimPrefix(r.host, "["), "]")
	if cfg.Port > 0 {
		r.port = cfg.Port
	}
	if executable, err := nativeSSHExecutable(); err == nil {
		configCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		args := []string{"-G", "-o", "CanonicalizeHostname=no", "-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none"}
		if cfg.Port > 0 {
			args = append(args, "-p", strconv.Itoa(cfg.Port))
		}
		args = append(args, "--", cfg.Target)
		output, err := nativeSSHConfig(configCtx, executable, args)
		cancel()
		if err != nil {
			return r, fmt.Errorf("read OpenSSH configuration: %w", err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(output)))
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), " ")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch key {
			case "hostname":
				r.host = value
			case "user":
				r.user = value
			case "port":
				r.port, err = strconv.Atoi(value)
				if err != nil {
					return r, errors.New("invalid configured SSH port")
				}
			case "identityfile":
				r.identities = append(r.identities, expandSSHPath(value, home))
			case "identityagent":
				if value != "SSH_AUTH_SOCK" {
					r.agentPath = value
				}
			case "identitiesonly":
				r.identitiesOnly = value == "yes"
			case "hostkeyalias":
				if value != "none" {
					r.hostKeyAlias = value
				}
			case "proxycommand", "proxyjump":
				if value != "none" && value != "" {
					return r, errors.New("Windows native SSH does not yet support ProxyCommand or ProxyJump; connect directly to the host")
				}
			case "userknownhostsfile", "globalknownhostsfile":
				paths, pathErr := configKnownPaths(value, home)
				if pathErr != nil {
					return r, pathErr
				}
				if key == "userknownhostsfile" && len(paths) > 0 {
					r.knownWrite = paths[0]
				}
				r.knownFiles = append(r.knownFiles, paths...)
			}
		}
		if err := scanner.Err(); err != nil {
			return r, err
		}
	}
	if r.user == "" || !namePattern.MatchString(r.user) {
		return r, errors.New("provide a valid remote Linux username as user@host")
	}
	if err := ValidateTarget(r.host); err != nil {
		return r, fmt.Errorf("configured SSH hostname: %w", err)
	}
	if r.port < 1 || r.port > 65535 {
		return r, errors.New("invalid configured SSH port")
	}
	if len(r.identities) == 0 {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			r.identities = append(r.identities, filepath.Join(home, ".ssh", name))
		}
	}
	if override := os.Getenv("RELAY_SSH_KNOWN_HOSTS"); override != "" {
		if !filepath.IsAbs(override) {
			return r, errors.New("RELAY_SSH_KNOWN_HOSTS must be an absolute path")
		}
		r.knownWrite = override
		r.knownFiles = []string{override}
	} else {
		found := false
		for _, path := range r.knownFiles {
			if path == r.knownWrite {
				found = true
				break
			}
		}
		if !found {
			r.knownFiles = append(r.knownFiles, r.knownWrite)
		}
	}
	if r.knownWrite == "" || strings.ContainsAny(r.knownWrite, "\r\n\x00") {
		return r, errors.New("invalid known-hosts file")
	}
	return r, nil
}

func expandSSHPath(value, home string) string {
	value = strings.Trim(value, `"`)
	value = strings.ReplaceAll(value, "%d", home)
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return filepath.Join(home, value[2:])
	}
	return value
}
func configKnownPaths(value, home string) ([]string, error) {
	// OpenSSH normally emits two whitespace-separated paths. Preserve a
	// configured single existing path containing spaces before splitting it.
	expanded := expandSSHPath(value, home)
	if _, err := os.Stat(expanded); err == nil {
		return []string{expanded}, nil
	}
	value = strings.ReplaceAll(value, home, "~")
	value = strings.ReplaceAll(value, filepath.ToSlash(home), "~")
	var paths []string
	var word strings.Builder
	var quote rune
	flush := func() {
		if word.Len() > 0 {
			path := word.String()
			if path != "none" {
				paths = append(paths, expandSSHPath(path, home))
			}
			word.Reset()
		}
	}
	for _, r := range value {
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case ' ', '\t':
			flush()
		default:
			word.WriteRune(r)
		}
	}
	if quote != 0 {
		return nil, errors.New("invalid quoted SSH known-hosts path")
	}
	flush()
	return paths, nil
}

func knownHostCheck(cfg nativeConfig) (ssh.HostKeyCallback, error) {
	var files []string
	for _, path := range cfg.knownFiles {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return nil, errors.New("known-hosts file must be a regular file under 8 MiB")
		}
		files = append(files, path)
	}
	return knownhosts.New(files...)
}

func (c *Connection) verifyHost(cfg nativeConfig, remote net.Addr, key ssh.PublicKey) error {
	check, err := knownHostCheck(cfg)
	if err != nil {
		return err
	}
	err = check(cfg.keyAddress(), remote, key)
	if err == nil {
		return nil
	}
	var mismatch *knownhosts.KeyError
	if !errors.As(err, &mismatch) || len(mismatch.Want) > 0 {
		return errors.New("SSH host key changed or is revoked; verify the server and update its known_hosts entry manually")
	}
	answer, err := c.ask(fmt.Sprintf("The authenticity of host '%s' cannot be established.\r\n%s key fingerprint is %s.\r\nAre you sure you want to continue connecting (yes/no)? ", cfg.keyAddress(), key.Type(), ssh.FingerprintSHA256(key)), true)
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) != "yes" {
		return errors.New("SSH host key was not accepted")
	}
	if err := os.MkdirAll(filepath.Dir(cfg.knownWrite), 0700); err != nil {
		return err
	}
	unlock, err := nativeKnownHostsLock(c.ctx, cfg.knownWrite+".relay-lock")
	if err != nil {
		return err
	}
	defer unlock()
	check, err = knownHostCheck(cfg)
	if err != nil {
		return err
	}
	err = check(cfg.keyAddress(), remote, key)
	if err == nil {
		return nil
	}
	if !errors.As(err, &mismatch) || len(mismatch.Want) > 0 {
		return errors.New("SSH host key changed while awaiting confirmation")
	}
	if info, err := os.Lstat(cfg.knownWrite); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("known-hosts destination must be a regular file")
	}
	file, err := os.OpenFile(cfg.knownWrite, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.WriteString(file, "\n"+knownhosts.Line([]string{cfg.keyAddress()}, key)+"\n"); err != nil {
		return err
	}
	return file.Sync()
}

func (c *Connection) authConfig(cfg nativeConfig) (*ssh.ClientConfig, func(), error) {
	var agentConn net.Conn
	var signers []ssh.Signer
	stopAgent := func() bool { return false }
	if !cfg.identitiesOnly && cfg.agentPath != "none" {
		ctx, cancel := context.WithTimeout(c.ctx, time.Second)
		agentConn, _ = nativeSSHAgent(ctx, cfg.agentPath)
		cancel()
		if agentConn != nil {
			stopAgent = context.AfterFunc(c.ctx, func() { _ = agentConn.Close() })
			_ = agentConn.SetDeadline(time.Now().Add(2 * time.Second))
			signers, _ = agent.NewClient(agentConn).Signers()
			_ = agentConn.SetDeadline(time.Time{})
		}
	}
	closeAuth := func() {
		stopAgent()
		if agentConn != nil {
			_ = agentConn.Close()
		}
	}
	auth := ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
		result := append([]ssh.Signer(nil), signers...)
		for _, path := range cfg.identities {
			if path == "none" {
				continue
			}
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return nil, errors.New("SSH private key must be a regular file under 1 MiB")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			signer, parseErr := ssh.ParsePrivateKey(data)
			var encrypted *ssh.PassphraseMissingError
			if errors.As(parseErr, &encrypted) {
				// Agent identities get first chance without prompting for an
				// encrypted key already unlocked by that agent.
				if encrypted.PublicKey != nil {
					found := false
					for _, existing := range result {
						if string(existing.PublicKey().Marshal()) == string(encrypted.PublicKey.Marshal()) {
							found = true
							break
						}
					}
					if found {
						clear(data)
						continue
					}
				}
				passphrase, promptErr := c.ask("Enter passphrase for key "+filepath.Base(path)+": ", false)
				if promptErr != nil {
					clear(data)
					return nil, promptErr
				}
				secret := []byte(passphrase)
				signer, parseErr = ssh.ParsePrivateKeyWithPassphrase(data, secret)
				clear(secret)
			}
			clear(data)
			if parseErr != nil {
				return nil, fmt.Errorf("read SSH identity %s: %w", filepath.Base(path), parseErr)
			}
			result = append(result, signer)
		}
		return result, nil
	})
	config := &ssh.ClientConfig{User: cfg.user, Timeout: 15 * time.Second, HostKeyCallback: func(_ string, remote net.Addr, key ssh.PublicKey) error { return c.verifyHost(cfg, remote, key) }, BannerCallback: func(message string) error {
		if len(message) > 16<<10 {
			message = message[:16<<10]
		}
		c.publish([]byte(message))
		return nil
	}}
	config.Auth = []ssh.AuthMethod{auth, ssh.RetryableAuthMethod(ssh.KeyboardInteractive(func(user, instruction string, questions []string, echo []bool) ([]string, error) {
		if len(questions) > 8 {
			return nil, errors.New("SSH server requested too many authentication answers")
		}
		if len(instruction) > 16<<10 {
			instruction = instruction[:16<<10]
		}
		if instruction != "" {
			c.publish([]byte(instruction + "\r\n"))
		}
		answers := make([]string, len(questions))
		for i, q := range questions {
			if len(q) > 4096 {
				return nil, errors.New("SSH authentication prompt is too large")
			}
			value, err := c.ask(q+" ", false)
			if err != nil {
				return nil, err
			}
			answers[i] = value
		}
		return answers, nil
	}), 3), ssh.RetryableAuthMethod(ssh.PasswordCallback(func() (string, error) { return c.ask(cfg.user+"@"+cfg.host+"'s password: ", false) }), 3)}
	return config, closeAuth, nil
}
