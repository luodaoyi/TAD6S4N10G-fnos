//go:build !windows

package powerguard

import "os"

func elevated() bool {
	return os.Geteuid() == 0
}
