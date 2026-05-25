package server

import (
	"context"
	"log"
	"os"
	"path/filepath"
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

			if event.Has(fsnotify.Create) {
				info, err := os.Stat(event.Name)
				if err == nil && info.IsDir() {
					if err := iw.addWatchRecursive(watcher, event.Name); err != nil {
						log.Printf("Failed to attach watcher to new directory %s: %v", event.Name, err)
					}
					continue
				}
			}

			if iw.ShouldIgnoreFile(event.Name, false) {
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

func (iw *IngestionWorker) addWatchRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.IsDir() {
			return nil
		}

		if iw.ShouldIgnoreFile(path, true) {
			if path == root {
				return nil
			}
			return filepath.SkipDir
		}

		return watcher.Add(path)
	})
}

func (iw *IngestionWorker) IngestionConsumer(ctx context.Context, eventChan <-chan string) {
	timers := make(map[string]*time.Timer)

	for {
		select {
		case <-ctx.Done():
			return
		case path := <-eventChan:
			if iw.ShouldIgnoreFile(path, false) {
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

				iw.SyncFileState(ctx, path)
			})
		}
	}
}
