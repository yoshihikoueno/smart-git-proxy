package cache

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvictionByMtime(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, 1024, nil)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}

	w1, err := c.NewWriter("repo1", KindPack, "k1")
	if err != nil {
		t.Fatalf("writer1: %v", err)
	}
	if _, err := w1.Write(make([]byte, 800)); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if err := w1.Commit(); err != nil {
		t.Fatalf("commit1: %v", err)
	}

	w2, err := c.NewWriter("repo1", KindPack, "k2")
	if err != nil {
		t.Fatalf("writer2: %v", err)
	}
	if _, err := w2.Write(make([]byte, 800)); err != nil {
		t.Fatalf("write2: %v", err)
	}
	if err := w2.Commit(); err != nil {
		t.Fatalf("commit2: %v", err)
	}

	total := dirSize(t, dir)
	if total > 1024 {
		t.Fatalf("eviction failed, size=%d", total)
	}
}

func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
