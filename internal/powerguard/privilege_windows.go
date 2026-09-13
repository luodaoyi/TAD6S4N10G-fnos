//go:build windows

package powerguard

func elevated() bool {
	// Windows builds are development-only; privilege checks are enforced on linux/amd64.
	return true
}
