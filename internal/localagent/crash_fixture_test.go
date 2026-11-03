package localagent

import (
	"fmt"
	"io"
	"os"
	"time"
)

func crashHostFixture() int {
	exe, _ := os.Executable()
	env := setEnv(os.Environ(), "LOCALAGENT_CRASH_HOST", "")
	env = setEnv(env, "LOCALAGENT_PROVIDER_FIXTURE", "1")
	cwd, _ := os.Getwd()
	p, err := startProcess(command{Path: exe, Args: []string{"--child"}, Dir: cwd, Env: env}, 80, 24)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	go io.Copy(os.Stdout, p)
	time.Sleep(2 * time.Minute)
	p.Close()
	return 0
}
