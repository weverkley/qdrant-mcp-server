package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// --- File Watcher & Ingestion Subsystems ---
func (iw *IngestionWorker) watchLoop(ctx context.Context, watcher *fsnotify.Watcher, eventChan chan<- string) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// NEVER ingest the log file
			if strings.Contains(event.Name, ".qdrant-mcp-server.log") {
				continue
			}

			baseName := filepath.Base(event.Name)

			// Dynamically determine if the event path matches any allowed hidden folder patterns
			isAllowedHiddenPath := false
			for _, allowedDir := range iw.cfg.IncludeHiddenDirs {
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

func (iw *IngestionWorker) ingestionConsumer(ctx context.Context, eventChan <-chan string) {
	timers := make(map[string]*time.Timer)

	for {
		select {
		case <-ctx.Done():
			return
		case path := <-eventChan:
			// NEVER ingest the log file
			if strings.Contains(path, ".qdrant-mcp-server.log") {
				continue
			}

			// Debounce frequent chunks to avoid thrashing remote CPU/Network resources
			if timer, exists := timers[path]; exists {
				timer.Stop()
			}

			iw.mu.Lock()
			iw.pendingFiles[path] = time.Now()
			iw.mu.Unlock()

			timers[path] = time.AfterFunc(iw.cfg.DebounceDuration, func() {
				iw.mu.Lock()
				delete(iw.pendingFiles, path)
				iw.activeSyncs++
				iw.mu.Unlock()

				defer func() {
					iw.mu.Lock()
					iw.activeSyncs--
					iw.totalSynced++
					iw.mu.Unlock()
				}()

				iw.syncFileState(context.Background(), path)
			})
		}
	}
}
