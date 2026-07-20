package gotorrent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimCacheRemovesOldestEntriesFirst(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old")
	newPath := filepath.Join(root, "new")
	writeCacheTestFile(t, oldPath, 64)
	writeCacheTestFile(t, newPath, 64)
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	stats := reclaimCacheLocked(root, 64)
	if stats.ReclaimedBytes != 64 {
		t.Fatalf("expected 64 reclaimed bytes, got %d", stats.ReclaimedBytes)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected oldest entry to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected newest entry to remain: %v", err)
	}
}

func TestClearCacheRemovesAllInactiveEntries(t *testing.T) {
	root := t.TempDir()
	writeCacheTestFile(t, filepath.Join(root, "one"), 32)
	writeCacheTestFile(t, filepath.Join(root, "two"), 48)

	stats := reclaimCacheLocked(root, 0)
	if stats.UsedBytes != 0 {
		t.Fatalf("expected empty cache, got %d bytes", stats.UsedBytes)
	}
	if stats.ReclaimedBytes != 80 {
		t.Fatalf("expected 80 reclaimed bytes, got %d", stats.ReclaimedBytes)
	}
}

func writeCacheTestFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}
