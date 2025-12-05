package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"log/slog"
)

type Kind string

const (
	KindInfo Kind = "info"
	KindPack Kind = "pack"
)

type Cache struct {
	root     string
	maxBytes int64
	logger   *slog.Logger

	repoLocks sync.Map // map[string]*sync.Mutex
}

type Entry struct {
	Path string
	Size int64
}

type Writer struct {
	cache   *Cache
	temp    string
	final   string
	closed  bool
	written int64
	file    *os.File
}

func New(root string, maxBytes int64, logger *slog.Logger) (*Cache, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Cache{root: root, maxBytes: maxBytes, logger: logger}, nil
}

func (c *Cache) Get(repo string, kind Kind, key string) (*os.File, *Entry, error) {
	path := c.entryPath(repo, kind, key)
	stat, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	_ = os.Chtimes(path, time.Now(), time.Now())
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return f, &Entry{Path: path, Size: stat.Size()}, nil
}

func (c *Cache) NewWriter(repo string, kind Kind, key string) (*Writer, error) {
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()

	dir := filepath.Dir(c.entryPath(repo, kind, key))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tempFile, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return nil, err
	}
	return &Writer{
		cache: c,
		temp:  tempFile.Name(),
		final: c.entryPath(repo, kind, key),
		file:  tempFile,
	}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("writer closed")
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

func (w *Writer) Commit() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if err := w.file.Sync(); err != nil {
		return err
	}
	if err := w.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(w.temp, w.final); err != nil {
		return err
	}
	_ = w.cache.evict()
	return nil
}

func (w *Writer) Abort() {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.file.Close()
	_ = os.Remove(w.temp)
}

func (c *Cache) entryPath(repo string, kind Kind, key string) string {
	repoID := hashString(repo)
	keyID := hashString(key)
	switch kind {
	case KindInfo:
		return filepath.Join(c.root, "info", repoID, keyID+".pkt")
	case KindPack:
		return filepath.Join(c.root, "objects", "pack", repoID, keyID+".pack")
	default:
		return filepath.Join(c.root, "misc", repoID, keyID)
	}
}

func (c *Cache) repoLock(repo string) *sync.Mutex {
	if v, ok := c.repoLocks.Load(repo); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := c.repoLocks.LoadOrStore(repo, mu)
	return actual.(*sync.Mutex)
}

func (c *Cache) evict() error {
	if c.maxBytes <= 0 {
		return nil
	}
	var files []fileInfo
	var total int64
	root := c.root

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		files = append(files, fileInfo{path: path, modTime: info.ModTime(), size: info.Size()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}

	if total <= c.maxBytes {
		return nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	for _, f := range files {
		if total <= c.maxBytes {
			break
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
		} else if c.logger != nil {
			c.logger.Warn("evict failed", "path", f.path, "err", err)
		}
	}
	return nil
}

type fileInfo struct {
	path    string
	modTime time.Time
	size    int64
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
