package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// DirItem is one name in a folder listing.
type DirItem struct {
	Name  string `json:"n"`
	IsDir bool   `json:"d,omitempty"`
}

// DirCache remembers folder listings by the folder's modified time.
// Listing a folder is slow on some machines (about 0.5 ms per folder here,
// against microseconds for a stat), and a folder's modified time changes
// whenever an entry is added, removed or renamed in it. So a listing is
// read again only when the time changed.
type DirCache struct {
	mu    sync.Mutex
	dirs  map[string]CachedDir
	seen  map[string]bool
	dirty map[string]bool
}

// CachedDir is one remembered listing.
type CachedDir struct {
	ModTime int64
	Items   []DirItem
}

// NewDirCache starts a cache from saved listings.
func NewDirCache(saved map[string]CachedDir) *DirCache {
	if saved == nil {
		saved = map[string]CachedDir{}
	}
	return &DirCache{dirs: saved, seen: map[string]bool{}, dirty: map[string]bool{}}
}

// ReadDir lists a folder, from the cache when its modified time is
// unchanged. A missing folder gives no items and no error.
func (c *DirCache) ReadDir(path string) ([]DirItem, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	mtime := info.ModTime().UnixNano()
	if c != nil {
		c.mu.Lock()
		c.seen[path] = true
		if d, ok := c.dirs[path]; ok && d.ModTime == mtime {
			c.mu.Unlock()
			return d.Items, nil
		}
		c.mu.Unlock()
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	items := make([]DirItem, len(entries))
	for i, e := range entries {
		items[i] = DirItem{Name: e.Name(), IsDir: e.IsDir()}
	}
	if c != nil {
		c.mu.Lock()
		c.dirs[path] = CachedDir{ModTime: mtime, Items: items}
		c.dirty[path] = true
		c.mu.Unlock()
	}
	return items, nil
}

// Changed returns the listings read again in this run, and the cached
// folders that weren't visited (gone, or no longer walked).
func (c *DirCache) Changed() (save map[string]CachedDir, forget []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	save = map[string]CachedDir{}
	for p := range c.dirty {
		save[p] = c.dirs[p]
	}
	for p := range c.dirs {
		if !c.seen[p] {
			forget = append(forget, p)
		}
	}
	return save, forget
}

// ReadDir lists a folder through the env's cache, if it has one.
func (e Env) ReadDir(path string) ([]DirItem, error) {
	return e.Dirs.ReadDir(path)
}

// Walk calls fn for every file under root (not folders), using the env's
// folder cache. keepDir, if not nil, decides whether to enter a folder.
// A missing root is not an error.
func (e Env) Walk(root string, keepDir func(path string) bool, fn func(path string) error) error {
	items, err := e.ReadDir(root)
	if err != nil {
		return err
	}
	for _, it := range items {
		p := filepath.Join(root, it.Name)
		if it.IsDir {
			if keepDir != nil && !keepDir(p) {
				continue
			}
			if err := e.Walk(p, keepDir, fn); err != nil {
				return err
			}
			continue
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

// StatFile returns a File for path with its stat fingerprint, or ok false
// when the file is gone.
func StatFile(path string) (File, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return File{}, false
	}
	return File{Path: path, Fingerprint: StatFingerprint(info)}, true
}
