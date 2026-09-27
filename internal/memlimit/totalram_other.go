//go:build !linux

package memlimit

import "errors"

func totalram() (uint64, error) {
	return 0, errors.New("not read outside linux")
}
