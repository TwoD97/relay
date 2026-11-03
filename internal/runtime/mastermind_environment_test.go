//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMastermindCapabilityNotInheritedByOrdinaryEnvironment(t *testing.T) {
	t.Setenv("RELAY_CONTROLLER_STATE", "/private/controller")
	t.Setenv("RELAY_AGENT_CAPABILITY_FILE", "/private/controller/agent-capabilities/private.json")
	t.Setenv("LOCALAGENT_TEST_PROVIDER_CONFIG", "preserved")
	s := &Server{stateDir: t.TempDir(), home: t.TempDir()}
	env := s.environment()
	if envValue(env, "RELAY_CONTROLLER_STATE") != "" || envValue(env, "RELAY_AGENT_CAPABILITY_FILE") != "" {
		t.Fatal("ordinary runtime environment inherited a controller capability")
	}
	if envValue(env, "LOCALAGENT_TEST_PROVIDER_CONFIG") != "preserved" {
		t.Fatal("provider configuration was modified")
	}
	exe, _ := os.Executable()
	if envValue(env, "RELAY_BIN") != exe {
		t.Fatal("own runtime binary missing")
	}
	id := "0123456789abcdef0123456789abcdef"
	values := map[string]string{"RELAY_CONTROLLER_STATE": s.stateDir, "RELAY_AGENT_CAPABILITY_FILE": filepath.Join(s.stateDir, "agent-capabilities", id+".json"), "RELAY_BIN": exe}
	if err := validateMastermindEnvironment(id, values); err != nil {
		t.Fatal(err)
	}
	for k, v := range values {
		env = setEnv(env, k, v)
	}
	if envValue(env, "RELAY_AGENT_CAPABILITY_FILE") != values["RELAY_AGENT_CAPABILITY_FILE"] {
		t.Fatal("explicit capability could not be restored")
	}
}
