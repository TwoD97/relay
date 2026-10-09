//go:build !linux

package projectcontext

import (
	"context"
	"errors"
)

func Prepare(ctx context.Context, path string) (Result, error) {
	return Result{}, errors.New("shared project context requires a Linux host; select a connected Linux host")
}
