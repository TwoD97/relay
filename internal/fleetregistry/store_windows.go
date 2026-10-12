package fleetregistry

import (
	"context"
	"errors"
	"os"
)

func owned(_ os.FileInfo) error { return nil }
func lock(_ context.Context, _ string) (func(), error) {
	return nil, errors.New("fleet registry imports run on the Linux runtime host")
}
func syncDirectory(_ string) error { return nil }
