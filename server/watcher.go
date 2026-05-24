package server

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// --- File Watcher & Ingestion Subsystems ---
func (iw *IngestionWorker) WatchLoop(ctx context.Context, watcher *fsnotify.Watcher, eventChan chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// NEVER ingest the log file or its dedicated directory files
			if strings.Contains(event.Name, ".qdrant-mcp-server") {
				continue
			}

			baseName := filepath.Base(event.Name)

			// Dynamically determine if the event path matches any allowed hidden folder patterns
			isAllowedHiddenPath := false
			for _, allowedDir := range iw.Cfg.IncludeHiddenDirs {
				if strings.Contains(event.Name, "/"+allowedDir+"/") {
					isAllowedHiddenPath = true
					break
				}
			}

			// Guard against general hidden files unless they belong to an explicitly allowed tree
			if (strings.HasPrefix(baseName, ".") && !isAllowedHiddenPath) ||
				strings.HasPrefix(baseName, "~") ||
				strings.HasSuffix(baseName, ".tmp") {
				continue
			}

			// Focus explicitly on functional mutation blocks
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove) {
				eventChan <- event.Name
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("Fsnotify interface raised exception: %v", err)
		}
	}
}

func (iw *IngestionWorker) IngestionConsumer(ctx context.Context, eventChan <-chan string) {
	timers := make(map[string]*time.Timer)

	for {
		select {
		case <-ctx.Done():
			return
		case path := <-eventChan:
			// NEVER ingest the log file or its dedicated directory files
			if strings.Contains(path, ".qdrant-mcp-server") {
				continue
			}

			// Debounce frequent chunks to avoid thrashing remote CPU/Network resources
			if timer, exists := timers[path]; exists {
				timer.Stop()
			}

			iw.Mu.Lock()
			iw.PendingFiles[path] = time.Now()
			iw.Mu.Unlock()

			timers[path] = time.AfterFunc(iw.Cfg.DebounceDuration, func() {
				iw.Mu.Lock()
				delete(iw.PendingFiles, path)
				iw.ActiveSyncs++
				iw.Mu.Unlock()

				defer func() {
					iw.Mu.Lock()
					iw.ActiveSyncs--
					iw.TotalSynced++
					iw.Mu.Unlock()
				}()

				iw.SyncFileState(context.Background(), path)
			})
		}
	}
}
