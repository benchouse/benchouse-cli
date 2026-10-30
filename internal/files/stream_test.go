package files

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/benchouse/benchouse-cli/internal/bundle"
)

// A folder far larger than what may sit in memory goes through Walk into a
// staging file and into the bundle while the heap stays at about one file.
func TestManyFilesStreamThroughStagingWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("writes and masks 31 MiB")
	}
	const n, size = 2000, 16 << 10 // 31 MiB in total
	root := filepath.Join(t.TempDir(), "proj")
	line := "select order_id, sum(amount) as revenue from orders group by 1 -- model %05d\n"
	for i := 0; i < n; i++ {
		dir := filepath.Join(root, "models", fmt.Sprintf("d%02d", i%40))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		one := fmt.Sprintf(line, i)
		body := strings.Repeat(one, size/len(one)+1)[:size]
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("m%05d.sql", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	st, err := bundle.NewStaging(out)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapInuse, ms.HeapInuse
	sample := func() {
		runtime.ReadMemStats(&ms)
		peak = max(peak, ms.HeapInuse)
	}
	count := 0
	if _, err := Walk(root, func(rel string) bool { return strings.HasSuffix(rel, ".sql") }, func(f File) error {
		if _, err := st.Add("dbt/"+f.Rel, f.Data, f.Masked); err != nil {
			return err
		}
		if count++; count%100 == 0 {
			sample()
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st.SortFrom(0)
	sample()
	dst := filepath.Join(out, "b.tar.gz")
	m := bundle.Manifest{CreatedAt: time.Unix(0, 0).UTC(), Files: st.Files()}
	if err := bundle.Write(context.Background(), dst, false, m, nil, st); err != nil {
		t.Fatal(err)
	}
	sample()
	const total = n * size
	if grown := int64(peak) - int64(base); grown > total/4 {
		t.Errorf("heap grew %d MiB for %d MiB of files: the folder was held in memory", grown>>20, total>>20)
	}
	s, err := bundle.Inspect(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Manifest.Files) != n {
		t.Errorf("bundle holds %d files, want %d", len(s.Manifest.Files), n)
	}
}
