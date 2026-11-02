//go:build linux

package podsupervisor

import (
	"reflect"
	"strings"
	"testing"
)

func TestSysvIDs(t *testing.T) {
	table := `       key      shmid perms                  size  cpid  lpid nattch   uid   gid  cuid  cgid      atime      dtime      ctime                   rss                  swap
1627271612          0   644                  4096     8     0     0  1001  2000  1001  2000          0          0 1762098955                     0                     0
1627271613         32   600                  8192     9     0     0  1001  2000  1001  2000          0          0 1762098956                     0                     0
`
	got := sysvIDs(strings.NewReader(table))
	if want := []int{0, 32}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if got := sysvIDs(strings.NewReader("       key      shmid perms\n")); len(got) != 0 {
		t.Fatalf("an empty table gave %v", got)
	}
}
