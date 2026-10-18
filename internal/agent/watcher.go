package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/utils"
)

func (e *Engine) hasStaleFiles() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.staleFiles) > 0
}

func (e *Engine) markStale(absPath string) {
	rel, err := filepath.Rel(e.root, absPath)
	if err != nil {
		return
	}

	// Avoid indexing hidden files or loa internal files
	if strings.HasPrefix(rel, ".") || strings.Contains(rel, string(filepath.Separator)+".") {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	// ModTime check to ignore ghost fsnotify events, and skip directory paths
	if stat, err := os.Stat(absPath); err == nil {
		if stat.IsDir() {
			return
		}
		if e.reconciledModTimes != nil {
			if lastMod, ok := e.reconciledModTimes[rel]; ok {
				if stat.ModTime().UnixNano() <= lastMod.UnixNano() {
					return
				}
			}
		}
	}

	if e.staleFiles == nil {
		e.staleFiles = make(map[string]bool)
	}

	e.staleFiles[rel] = true
	e.invalidatedPaths.Store(rel, true)
}

func (e *Engine) maybeRepairStaleFilesWhileIdle(ctx context.Context) {
	e.mu.Lock()
	if e.running || e.needsProjectSetup || len(e.staleFiles) == 0 {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.systemTaskReason = "indexing"
	e.mu.Unlock()
	defer e.UnlockSystemTask()

	for e.hasStaleFiles() {
		if err := e.repairStaleFiles(ctx); err != nil {
			e.log(state.LogSystem, "Watcher", "idle stale file repair failed", "", err.Error())
			return
		}
	}
}

func (e *Engine) repairStaleFiles(ctx context.Context) error {
	e.mu.Lock()
	if len(e.staleFiles) == 0 {
		e.mu.Unlock()
		return nil
	}

	files := make([]string, 0, len(e.staleFiles))
	for f := range e.staleFiles {
		files = append(files, f)
	}
	// Clear the set
	e.staleFiles = make(map[string]bool)
	e.mu.Unlock()

	_, isIgnored := utils.ReadLoaignore(e.root)
	var filtered []string
	for _, f := range files {
		if !isIgnored(f) {
			filtered = append(filtered, f)
		}
	}

	if len(filtered) == 0 {
		return nil
	}

	e.mu.Lock()
	if e.reconciledModTimes == nil {
		e.reconciledModTimes = make(map[string]time.Time)
	}
	for _, f := range filtered {
		if stat, err := os.Stat(filepath.Join(e.root, f)); err == nil {
			e.reconciledModTimes[f] = stat.ModTime()
		} else {
			delete(e.reconciledModTimes, f)
		}
	}
	e.mu.Unlock()

	e.log(state.LogSystem, "Watcher", "Just-In-Time reconciliation triggered for stale files", strings.Join(filtered, ", "), "")

	for _, file := range filtered {
		_ = e.memStore.RemoveMemoriesByAnchor(file)
	}

	if e.index == nil {
		return nil
	}

	// Refresh CTAGS index synchronously
	if err := e.index.Rebuild(ctx); err != nil {
		e.log(state.LogSystem, "Watcher", "failed to rebuild index during repair", "", err.Error())
	}

	// RunCrawler seamlessly re-indexes the now missing (stale) files
	return e.RunCrawler(ctx)
}

// StartWatcher initializes a recursive directory watcher and starts
// an event listener that flags modified files for healing.
func (e *Engine) StartWatcher(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	_, isIgnored := utils.ReadLoaignore(e.root)

	err = filepath.Walk(e.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			rel, _ := filepath.Rel(e.root, path)
			if rel != "." {
				if strings.HasPrefix(rel, ".") || strings.Contains(rel, string(filepath.Separator)+".") {
					return filepath.SkipDir
				}
				if isIgnored(rel) {
					return filepath.SkipDir
				}
			}
			return watcher.Add(path)
		}
		return nil
	})

	if err != nil {
		watcher.Close()
		return err
	}

	go func() {
		defer watcher.Close()

		// Debounce timer
		var timer *time.Timer
		var timerMu sync.Mutex
		events := make(map[string]bool)

		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}

				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
					if strings.Contains(filepath.Base(event.Name), ".loa-write-") {
						continue
					}

					if strings.Contains(event.Name, string(filepath.Separator)+".loa"+string(filepath.Separator)+"attachments"+string(filepath.Separator)) {
						go e.healAttachment(event.Name)
						continue
					}

					_, isIgnoredNow := utils.ReadLoaignore(e.root)
					rel, err := filepath.Rel(e.root, event.Name)
					if err == nil && rel != "." {
						if strings.HasPrefix(rel, ".") || strings.Contains(rel, string(filepath.Separator)+".") {
							continue
						}
						if isIgnoredNow(rel) {
							continue
						}
					}

					var discoveredFiles []string
					isCreatedDir := false
					if event.Op&fsnotify.Create == fsnotify.Create {
						if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
							isCreatedDir = true
							_ = filepath.Walk(event.Name, func(subPath string, subInfo os.FileInfo, walkErr error) error {
								if walkErr != nil {
									return nil
								}
								subRel, relErr := filepath.Rel(e.root, subPath)
								if relErr == nil && subRel != "." {
									if strings.HasPrefix(subRel, ".") || strings.Contains(subRel, string(filepath.Separator)+".") {
										if subInfo.IsDir() {
											return filepath.SkipDir
										}
										return nil
									}
									if isIgnoredNow(subRel) {
										if subInfo.IsDir() {
											return filepath.SkipDir
										}
										return nil
									}
								}
								if subInfo.IsDir() {
									_ = watcher.Add(subPath)
									return nil
								}
								discoveredFiles = append(discoveredFiles, subPath)
								return nil
							})
						}
					}

					timerMu.Lock()
					if isCreatedDir {
						for _, df := range discoveredFiles {
							events[df] = true
						}
					} else {
						events[event.Name] = true
					}
					if len(events) == 0 {
						timerMu.Unlock()
						continue
					}
					if timer != nil {
						timer.Stop()
					}
					debounceMs := e.cfg.Get().FSWatcherDebounceMs
					timer = time.AfterFunc(time.Duration(debounceMs)*time.Millisecond, func() {
						timerMu.Lock()
						pending := make([]string, 0, len(events))
						for f := range events {
							pending = append(pending, f)
						}
						// Clear the batch
						for k := range events {
							delete(events, k)
						}
						timerMu.Unlock()

						for _, f := range pending {
							e.markStale(f)
						}
						e.log(state.LogSystem, "Watcher", "fsnotify event triggered cache invalidation", strings.Join(pending, ", "), "")
						e.maybeRepairStaleFilesWhileIdle(ctx)
					})
					timerMu.Unlock()
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				e.log(state.LogSystem, "Watcher", "fsnotify error", "", err.Error())
			}
		}
	}()

	e.log(state.LogSystem, "Watcher", "started OS file watcher", "", "")
	return nil
}

func (e *Engine) healAttachment(path string) {
	snap := e.store.Snapshot()
	filename := filepath.Base(path)
	
	var masterPath string
	var readOnly bool
	for _, att := range snap.ActiveAttachments {
		if filepath.Base(att.VirtualPath) == filename {
			if att.Type == state.AttachmentTypeUpload {
				for _, up := range snap.Uploads {
					if up.ID == att.ID {
						masterPath = up.OriginalPath
						readOnly = up.ReadOnly
						break
					}
				}
			} else {
				masterPath = filepath.Join(e.root, att.ID)
				readOnly = true // Artifacts are always read-only
			}
			break
		}
	}
	
	if masterPath == "" {
		return 
	}
	
	if readOnly {
		// Revert: Master -> Sandbox
		sourceFile, err := os.Open(masterPath)
		if err != nil {
			return
		}
		defer sourceFile.Close()
		
		destFile, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return
		}
		defer destFile.Close()
		
		io.Copy(destFile, sourceFile)
		e.log(state.LogSystem, "Watcher", "auto-healed read-only attachment modified by agent", filename, "")
	} else {
		// Sync: Sandbox -> Master
		sourceFile, err := os.Open(path)
		if err != nil {
			return
		}
		defer sourceFile.Close()
		
		destFile, err := os.OpenFile(masterPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return
		}
		defer destFile.Close()
		
		io.Copy(destFile, sourceFile)
		e.log(state.LogSystem, "Watcher", "synced writable attachment changes to master upload", filename, "")
	}
}
