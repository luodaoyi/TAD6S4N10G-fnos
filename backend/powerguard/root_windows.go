//go:build windows

package main

func requireRoot() error {
	// Windows builds are used only for development-time tests. The shipped
	// executable is always linux/amd64 and enforces uid 0 in root_unix.go.
	return nil
}
