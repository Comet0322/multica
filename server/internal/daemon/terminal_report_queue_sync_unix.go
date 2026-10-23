//go:build !windows

package daemon

import (
	"github.com/multica-ai/multica/server/internal/util"
)

func syncTerminalReportDir(path string) error {
	dir, err := util.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
