package agent

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

func TestIDSpoolSpillsAndReplaysInOrder(t *testing.T) {
	for _, n := range []int{0, 5, spoolInMemory, spoolInMemory + 1, 2*spoolInMemory + 7} {
		var s IDSpool
		for i := range n {
			if err := s.Add(fmt.Sprintf("id-%d\n\"x", i)); err != nil {
				t.Fatal(err)
			}
		}
		if s.Spilled() != (n > spoolInMemory) {
			t.Errorf("n=%d spilled=%v", n, s.Spilled())
		}
		var name string
		if s.f != nil {
			name = s.f.Name()
			if fi, err := os.Stat(name); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
				t.Errorf("spool file mode: %v %v", fi, err)
			}
		}
		for pass := 0; pass < 2; pass++ {
			got, batches := 0, 0
			err := s.Batches(BatchSize, func(ids []string) error {
				batches++
				if len(ids) > BatchSize {
					t.Fatalf("batch of %d", len(ids))
				}
				for _, id := range ids {
					if want := fmt.Sprintf("id-%d\n\"x", got); id != want {
						t.Fatalf("id %d = %q", got, id)
					}
					got++
				}
				return nil
			})
			if err != nil || got != n || batches != (n+BatchSize-1)/BatchSize {
				t.Errorf("n=%d pass %d: got %d in %d batches, %v", n, pass, got, batches, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
		if name != "" {
			if _, err := os.Stat(name); !os.IsNotExist(err) {
				t.Errorf("spool file left behind: %v", err)
			}
		}
	}
}
