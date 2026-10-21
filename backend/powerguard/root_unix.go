//go:build !windows

package main

import (
	"errors"
	"os"
)

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("root privileges are required")
	}
	return nil
}
