package kms

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// FileProvider reads MASTER_KEK versions from a plain-text file and watches it
// for appended entries via fsnotify.
//
// # File format
//
// Each line is a versioned key entry:
//
//	v1:<base64url-32-byte-key>
//	v2:<base64url-32-byte-key>
//
// Lines are appended to rotate. The highest vN becomes the active version.
// Blank lines and leading/trailing whitespace are ignored.
type FileProvider struct {
	path     string
	mu       sync.RWMutex
	versions map[int][32]byte
	current  int
}

// FromFile constructs a FileProvider and starts the fsnotify watcher goroutine.
// ctx controls the lifetime of the watcher.
func FromFile(ctx context.Context, path string) (*FileProvider, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("kms/file: resolve path %s: %w", path, err)
	}

	p := &FileProvider{path: abs, versions: make(map[int][32]byte)}
	if err := p.reload(); err != nil {
		return nil, err
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("kms/file: create watcher: %w", err)
	}
	if err := w.Add(filepath.Dir(abs)); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("kms/file: watch dir %s: %w", filepath.Dir(abs), err)
	}

	go p.watch(ctx, w)
	return p, nil
}

// MasterKEK implements MasterKEKProvider.
func (p *FileProvider) MasterKEK(_ context.Context, version int) ([]byte, int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if version == LatestVersion {
		version = p.current
	}
	key, ok := p.versions[version]
	if !ok {
		return nil, 0, fmt.Errorf("kms/file: version %d not found (file has v1–v%d)", version, p.current)
	}
	out := make([]byte, 32)
	copy(out, key[:])
	return out, version, nil
}

func (p *FileProvider) reload() error {
	raw, err := os.ReadFile(p.path)
	if err != nil {
		return fmt.Errorf("kms/file: read %s: %w", p.path, err)
	}

	versions := make(map[int][32]byte)
	maxVer := 0

	for lineNum, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		ver, key, err := parseLine(line)
		if err != nil {
			return fmt.Errorf("kms/file: %s line %d: %w", p.path, lineNum+1, err)
		}
		if _, dup := versions[ver]; dup {
			return fmt.Errorf("kms/file: %s line %d: duplicate version v%d", p.path, lineNum+1, ver)
		}
		versions[ver] = key
		if ver > maxVer {
			maxVer = ver
		}
	}

	if maxVer == 0 {
		return fmt.Errorf("kms/file: %s contains no valid key entries", p.path)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.versions = versions
	p.current = maxVer
	return nil
}

func parseLine(line string) (version int, key [32]byte, err error) {
	if !strings.HasPrefix(line, "v") {
		return 0, key, fmt.Errorf("expected format v<N>:<key>, got %q", line)
	}
	colonIdx := strings.IndexByte(line, ':')
	if colonIdx < 2 {
		return 0, key, fmt.Errorf("expected format v<N>:<key>, got %q", line)
	}
	ver, err := strconv.Atoi(line[1:colonIdx])
	if err != nil || ver < 1 {
		return 0, key, fmt.Errorf("invalid version number in %q", line)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(line[colonIdx+1:]))
	if err != nil {
		return 0, key, fmt.Errorf("v%d key is not valid base64url: %w", ver, err)
	}
	if len(decoded) != 32 {
		return 0, key, fmt.Errorf("v%d key must decode to 32 bytes, got %d", ver, len(decoded))
	}
	copy(key[:], decoded)
	return ver, key, nil
}

func (p *FileProvider) watch(ctx context.Context, w *fsnotify.Watcher) {
	defer func() {
		_ = w.Close()
	}()
	name := filepath.Base(p.path)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			if filepath.Base(event.Name) != name {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				if err := p.reload(); err != nil {
					fmt.Fprintf(os.Stderr, "kms/file: reload failed (keeping previous state): %v\n", err)
					continue
				}
				p.mu.RLock()
				v := p.current
				p.mu.RUnlock()
				fmt.Fprintf(os.Stderr, "kms/file: reloaded %s — active version v%d\n", p.path, v)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "kms/file: watcher error: %v\n", err)
		}
	}
}
