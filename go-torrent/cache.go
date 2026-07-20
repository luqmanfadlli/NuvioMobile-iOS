package gotorrent

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	cacheRootDir    string
	cacheLimitBytes int64
)

type CacheStats struct {
	UsedBytes      int64  `json:"usedBytes"`
	ProtectedBytes int64  `json:"protectedBytes"`
	ReclaimedBytes int64  `json:"reclaimedBytes"`
	LimitBytes     int64  `json:"limitBytes"`
	ActiveSessions int    `json:"activeSessions"`
	ErrorMessage   string `json:"errorMessage,omitempty"`
}

type cacheEntry struct {
	path       string
	name       string
	size       int64
	modifiedAt time.Time
	protected  bool
}

// GetCacheStatsJson returns the current cache usage without deleting data.
// The directory argument keeps this API usable before StartEngine is called.
func GetCacheStatsJson(dataDir string) string {
	mu.RLock()
	defer mu.RUnlock()
	root := resolveCacheRootLocked(dataDir)
	return marshalCacheStats(measureCacheLocked(root, cacheLimitBytes))
}

// ReclaimCache removes the oldest inactive torrent payloads until the retained
// cache is at or below maxBytes. Payloads owned by torrents currently loaded in
// the client are always protected.
func ReclaimCache(dataDir string, maxBytes int64) string {
	mu.Lock()
	defer mu.Unlock()
	if maxBytes < 0 {
		maxBytes = 0
	}
	root := resolveCacheRootLocked(dataDir)
	cacheRootDir = root
	cacheLimitBytes = maxBytes
	return marshalCacheStats(reclaimCacheLocked(root, maxBytes))
}

// ClearCache removes every inactive torrent payload. Active torrents remain
// protected as a final safety net, although the Kotlin layer only calls this
// operation while playback is idle.
func ClearCache(dataDir string) string {
	mu.Lock()
	defer mu.Unlock()
	root := resolveCacheRootLocked(dataDir)
	return marshalCacheStats(reclaimCacheLocked(root, 0))
}

func configureCacheLocked(dataDir string, maxBytes int64) CacheStats {
	if maxBytes < 0 {
		maxBytes = 0
	}
	cacheRootDir = filepath.Clean(dataDir)
	cacheLimitBytes = maxBytes
	if err := os.MkdirAll(cacheRootDir, 0o755); err != nil {
		return CacheStats{
			LimitBytes:   cacheLimitBytes,
			ErrorMessage: err.Error(),
		}
	}
	return reclaimCacheLocked(cacheRootDir, cacheLimitBytes)
}

func resolveCacheRootLocked(dataDir string) string {
	if strings.TrimSpace(dataDir) != "" {
		return filepath.Clean(dataDir)
	}
	return cacheRootDir
}

func activeTorrentHashesLocked() map[string]struct{} {
	active := make(map[string]struct{})
	if client == nil {
		return active
	}
	for _, t := range client.Torrents() {
		active[strings.ToLower(t.InfoHash().HexString())] = struct{}{}
	}
	return active
}

func measureCacheLocked(root string, limitBytes int64) CacheStats {
	entries, err := collectCacheEntriesLocked(root)
	stats := CacheStats{
		LimitBytes:     limitBytes,
		ActiveSessions: len(activeTorrentHashesLocked()),
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || root == "" {
			return stats
		}
		stats.ErrorMessage = err.Error()
		return stats
	}
	for _, entry := range entries {
		stats.UsedBytes += entry.size
		if entry.protected {
			stats.ProtectedBytes += entry.size
		}
	}
	return stats
}

func reclaimCacheLocked(root string, maxBytes int64) CacheStats {
	if maxBytes < 0 {
		maxBytes = 0
	}
	entries, err := collectCacheEntriesLocked(root)
	stats := CacheStats{
		LimitBytes:     maxBytes,
		ActiveSessions: len(activeTorrentHashesLocked()),
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || root == "" {
			return stats
		}
		stats.ErrorMessage = err.Error()
		return stats
	}

	var removable []cacheEntry
	for _, entry := range entries {
		stats.UsedBytes += entry.size
		if entry.protected {
			stats.ProtectedBytes += entry.size
		} else {
			removable = append(removable, entry)
		}
	}

	sort.Slice(removable, func(i, j int) bool {
		if removable[i].modifiedAt.Equal(removable[j].modifiedAt) {
			return removable[i].name < removable[j].name
		}
		return removable[i].modifiedAt.Before(removable[j].modifiedAt)
	})

	var deleteErrors []string
	for _, entry := range removable {
		if stats.UsedBytes <= maxBytes {
			break
		}
		if err := os.RemoveAll(entry.path); err != nil {
			deleteErrors = append(deleteErrors, err.Error())
			continue
		}
		stats.UsedBytes = (stats.UsedBytes - entry.size)
		stats.ReclaimedBytes += entry.size
	}

	if len(deleteErrors) > 0 {
		stats.ErrorMessage = strings.Join(deleteErrors, "; ")
	}
	return stats
}

func collectCacheEntriesLocked(root string) ([]cacheEntry, error) {
	if root == "" {
		return nil, os.ErrNotExist
	}
	children, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	active := activeTorrentHashesLocked()
	engineRunning := client != nil
	entries := make([]cacheEntry, 0, len(children))
	for _, child := range children {
		path := filepath.Join(root, child.Name())
		size, modifiedAt, err := measurePath(path)
		if err != nil {
			return nil, err
		}
		name := strings.ToLower(child.Name())
		_, protected := active[name]
		// Hidden root entries may be storage metadata. Keep them while the
		// engine is open. Legacy payload folders remain reclaimable because the
		// active storage is partitioned into info-hash directories.
		if engineRunning && strings.HasPrefix(name, ".") {
			protected = true
		}
		entries = append(entries, cacheEntry{
			path:       path,
			name:       child.Name(),
			size:       size,
			modifiedAt: modifiedAt,
			protected:  protected,
		})
	}
	return entries, nil
}

func measurePath(root string) (size int64, modifiedAt time.Time, err error) {
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.ModTime().After(modifiedAt) {
			modifiedAt = info.ModTime()
		}
		if !entry.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return
}

func marshalCacheStats(stats CacheStats) string {
	encoded, err := json.Marshal(stats)
	if err != nil {
		return `{"errorMessage":"Failed to encode cache statistics"}`
	}
	return string(encoded)
}
