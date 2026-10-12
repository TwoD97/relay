//go:build windows || relay_ssh_native

package transport

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeFleetEndpointResolvesIsolatedOpenSSHConfig(t *testing.T) {
	executable, err := nativeSSHExecutable()
	if err != nil {
		t.Skip("OpenSSH configuration reader is not installed")
	}
	configPath := filepath.Join(t.TempDir(), "fixture config")
	config := "Host desktop-only-alias\n  HostName 192.0.2.27\n  User fixture_operator\n  Port 2222\n  IdentityFile /fixture/private-key-not-exported\n  IdentityAgent /fixture/agent-not-exported\n"
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	readConfig := func(ctx context.Context, args []string) ([]byte, error) {
		return nativeSSHConfig(ctx, executable, append([]string{"-F", configPath}, args...))
	}
	for _, test := range []struct {
		name   string
		config Config
		target string
		port   int
	}{
		{"alias", Config{Target: "desktop-only-alias"}, "fixture_operator@192.0.2.27", 2222},
		{"explicit-user-and-port", Config{Target: "override@desktop-only-alias", Port: 2200}, "override@192.0.2.27", 2200},
	} {
		t.Run(test.name, func(t *testing.T) {
			// This is the same resolver/endpoint projection used by FleetEndpoint;
			// only the configuration reader is restricted to a disposable -F file.
			resolved, err := resolveNativeConfigWith(context.Background(), test.config, readConfig)
			if err != nil {
				t.Fatal(err)
			}
			target, port, err := fleetEndpoint(resolved.user, resolved.host, resolved.port)
			if err != nil || target != test.target || port != test.port {
				t.Fatal("native alias resolution", target, port, err)
			}
		})
	}
}

func TestNativeFleetEndpointRejectsInvalidResolvedConfiguration(t *testing.T) {
	for _, output := range []string{
		"hostname 192.0.2.27\nuser fixture\nport not-a-port\n",
		"hostname bad host\nuser fixture\nport 22\n",
		"hostname 192.0.2.27\nuser fixture\nport 22\nproxyjump intermediary\n",
	} {
		_, err := resolveNativeConfigWith(context.Background(), Config{Target: "desktop-only-alias"}, func(context.Context, []string) ([]byte, error) {
			return []byte(output), nil
		})
		if err == nil {
			t.Fatal("invalid or unsupported resolved configuration was accepted")
		}
	}
}
