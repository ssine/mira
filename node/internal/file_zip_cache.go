package node

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ssine/mira/node/internal/resourcewire"
)

const zipPreviewBudget = int64(1 << 30)

type zipPreviewCacheEntry struct {
	path     string
	bytes    int64
	refs     int
	ready    chan struct{}
	err      error
	lastUsed time.Time
	timer    *time.Timer
}

func (runtime *capabilityRuntime) dropZIPCacheLocked(key string, e *zipPreviewCacheEntry) {
	if runtime.fileCache[key] != e {
		return
	}
	delete(runtime.fileCache, key)
	runtime.fileCacheBytes -= e.bytes
	if e.timer != nil {
		e.timer.Stop()
	}
	if e.path != "" {
		_ = os.Remove(e.path)
	}
}
func (runtime *capabilityRuntime) closeZIPCache() {
	runtime.fileCacheMu.Lock()
	defer runtime.fileCacheMu.Unlock()
	runtime.fileCacheClosed = true
	for key, e := range runtime.fileCache {
		if e.refs == 0 {
			runtime.dropZIPCacheLocked(key, e)
		}
	}
}
func (runtime *capabilityRuntime) releaseZIPCache(key string, e *zipPreviewCacheEntry) {
	runtime.fileCacheMu.Lock()
	defer runtime.fileCacheMu.Unlock()
	e.refs--
	e.lastUsed = time.Now()
	if e.refs != 0 {
		return
	}
	if e.err != nil || runtime.fileCacheClosed {
		runtime.dropZIPCacheLocked(key, e)
		return
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(5*time.Minute, func() {
		runtime.fileCacheMu.Lock()
		defer runtime.fileCacheMu.Unlock()
		if e.refs == 0 {
			runtime.dropZIPCacheLocked(key, e)
		}
	})
}
func (runtime *capabilityRuntime) openCachedZIPEntry(ctx context.Context, key string, entry *zip.File, directory string) (*os.File, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	runtime.fileCacheMu.Lock()
	if runtime.fileCacheClosed {
		runtime.fileCacheMu.Unlock()
		return nil, nil, fmt.Errorf("Node is shutting down")
	}
	if runtime.fileCache == nil {
		runtime.fileCache = map[string]*zipPreviewCacheEntry{}
	}
	e := runtime.fileCache[key]
	creator := e == nil
	if creator {
		if entry.UncompressedSize64 > uint64(zipPreviewBudget) {
			runtime.fileCacheMu.Unlock()
			return nil, nil, fmt.Errorf("selected ZIP entry exceeds 1 GiB temporary preview budget; download the original archive")
		}
		size := int64(entry.UncompressedSize64)
		for runtime.fileCacheBytes+size > zipPreviewBudget || len(runtime.fileCache) >= 64 {
			var oldest *zipPreviewCacheEntry
			oldKey := ""
			for k, item := range runtime.fileCache {
				if item.refs == 0 && (oldest == nil || item.lastUsed.Before(oldest.lastUsed)) {
					oldest = item
					oldKey = k
				}
			}
			if oldest == nil {
				runtime.fileCacheMu.Unlock()
				return nil, nil, fmt.Errorf("ZIP preview cache is busy; retry after other previews finish")
			}
			runtime.dropZIPCacheLocked(oldKey, oldest)
		}
		e = &zipPreviewCacheEntry{bytes: size, ready: make(chan struct{}), lastUsed: time.Now()}
		runtime.fileCache[key] = e
		runtime.fileCacheBytes += size
	}
	e.refs++
	if e.timer != nil {
		e.timer.Stop()
	}
	runtime.fileCacheMu.Unlock()
	release := func() { runtime.releaseZIPCache(key, e) }
	if creator {
		temp, err := os.CreateTemp(directory, "mira-preview-*")
		if err == nil {
			reader, openErr := entry.Open()
			err = openErr
			if err == nil {
				var written int64
				written, err = io.CopyBuffer(temp, io.LimitReader(cancelReader{ctx, reader}, int64(entry.UncompressedSize64)+1), make([]byte, resourcewire.FrameBytes))
				reader.Close()
				if err == nil && uint64(written) != entry.UncompressedSize64 {
					err = fmt.Errorf("ZIP size mismatch")
				}
			}
			if closeErr := temp.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				os.Remove(temp.Name())
			}
		}
		runtime.fileCacheMu.Lock()
		if temp != nil && err == nil {
			e.path = temp.Name()
		}
		e.err = err
		close(e.ready)
		runtime.fileCacheMu.Unlock()
	} else {
		select {
		case <-e.ready:
		case <-ctx.Done():
			release()
			return nil, nil, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, nil, err
	}
	if e.err != nil {
		release()
		return nil, nil, e.err
	}
	file, err := os.Open(e.path)
	if err != nil {
		release()
		return nil, nil, err
	}
	return file, func() { file.Close(); release() }, nil
}
