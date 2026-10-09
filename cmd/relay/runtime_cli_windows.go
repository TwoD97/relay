package main

import "errors"

func runtimeCommand(_ string, _ []string) error {
	return errors.New("terminal runtimes run on Linux hosts; connect an SSH host in the Windows Relay app")
}
