//go:build !windows

package daemon

import "os"

func syncTerminalReportDir(path string) error {
	// #nosec G304 -- path derives from operator config or a daemon-owned directory
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
