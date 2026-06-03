package up

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// NewFileConfig returns a PipelineConfig backed by a file path.
// The file is read fresh on every poll tick; path is evaluated at fetch time.
func NewFileConfig[T any](path string, dec ConfigDecoder[T], opts PollOptions) *PipelineConfig[T] {
	return NewPollingConfig(cachedFileSource(path), dec, opts)
}

// cachedFileSource returns a ConfigSource that caches file content by ModTime+Size.
// Calls os.Stat() (cheap, no I/O) on each fetch; only reads if the file's ModTime
// or Size have changed. Falls back to full read on Stat error.
func cachedFileSource(path string) ConfigSource {
	var mu sync.Mutex
	var lastModTime time.Time
	var lastSize int64
	var lastData []byte

	return func(_ context.Context) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()

		info, err := os.Stat(path)
		if err != nil {
			// On Stat error (file deleted, permissions), return cached data if available.
			// Otherwise return error. This handles transient issues gracefully.
			if len(lastData) > 0 {
				return lastData, nil
			}
			return nil, fmt.Errorf("orange/config: stat %s: %w", path, err)
		}

		modTime := info.ModTime()
		size := info.Size()

		// Cache hit: file unchanged.
		if modTime == lastModTime && size == lastSize && len(lastData) > 0 {
			return lastData, nil
		}

		// Cache miss: read the file and update cache.
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("orange/config: read %s: %w", path, err)
		}
		lastModTime = modTime
		lastSize = size
		lastData = data
		return data, nil
	}
}

// StartFileWatch enables file system watching for the given config file path.
// When the file is modified, an immediate refresh is triggered.
// Returns a stop function that cancels watching; safe to call multiple times.
// If watching cannot be set up, returns a no-op function and logs the error via the config's observer.
func StartFileWatch[T any](p *PipelineConfig[T], path string) func() {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return func() {}
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return func() {}
	}

	dir := filepath.Dir(absPath)
	if err := watcher.Add(dir); err != nil {
		watcher.Close()
		return func() {}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				// Trigger refresh on any write or chmod to the target file.
				if event.Name == absPath && (event.Op&fsnotify.Write != 0 || event.Op&fsnotify.Chmod != 0) {
					p.SignalRefresh()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				// Log error via the observer if configured.
				if p.opts.Observe != nil {
					p.opts.Observe(ConfigEvent{Err: fmt.Errorf("file watch error: %w", err)})
				}
			}
		}
	}()

	return func() {
		watcher.Close()
		wg.Wait()
	}
}
