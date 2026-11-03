package main

import (
	"errors"
	"os"
)

// Open the object itself without following a final link; nonblocking platform
// flags ensure a swapped FIFO/device cannot hang before the regular-file check.
func openAgentRead(path string, private bool) (*os.File, error) {
	f, err := openAgentFile(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("input must be a regular file, not a pipe or device")
	}
	if err == nil && private {
		err = verifyAgentFileOwner(f, info)
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
