package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/laughingmandev/loa/internal/agent"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/server"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
	"github.com/laughingmandev/loa/internal/version"
)

func main() {
	fmt.Printf("Starting Loa %s...\n", version.String)
	
	addr := flag.String("addr", "127.0.0.1:7171", "local HTTP listen address")
	flag.Parse()
	if flag.NArg() > 1 {
		log.Fatalf("usage: loa [-addr 127.0.0.1:7171] [project-root]")
	}
	root := "."
	if flag.NArg() == 1 {
		root = flag.Arg(0)
	}
	var err error
	root, err = filepath.Abs(root)
	if err != nil {
		log.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		log.Fatalf("resolve project root: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		log.Fatalf("project root is not a directory: %s", root)
	}

	cfgValue, err := config.Load(root)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cfg := config.NewManager(cfgValue)
	st, err := state.Load(root)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}
	// Loa now supports resuming in-flight execution after process death.
	// We intentionally leave any ActiveTask untouched on startup so it can be resumed.
	cv := cfg.Get()
	mem := memory.NewStore(root, cv.MemoryLTSDecayHalfLifeHours, cv.MemoryLTSMaxWeight)
	if err := mem.Load(); err != nil {
		log.Fatalf("load memory: %v", err)
	}

	idx := index.New(root)
	lc := llm.New(cfg.Get)
	tm := tools.New(root, idx, cfg.Get, lc)
	eng := agent.New(root, st, mem, cfg, lc, tm, idx)
	srv, err := server.New(root, *addr, st, cfg, lc, eng, idx)
	if err != nil {
		log.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Run() }()
	fmt.Printf("Loa is running at %s\nProject: %s\n", srv.URL(), root)

	eng.LockSystemTask("indexing")

	idxCtx, cancelIdx := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := idx.Rebuild(idxCtx); err != nil {
		log.Printf("symbol index unavailable: %v", err)
	} else {
		log.Printf("symbol index ready: %d files", len(idx.Snapshot()))
	}
	cancelIdx()

	c := cfg.Get()
	hasEmbedding := c.EmbeddingModel != "" || (c.EmbeddingEngine == "local" && c.LocalEmbeddingModelPath != "")
	if len(st.SessionStatus().Sessions) == 0 {
		eng.SetNeedsProjectSetup(true)
		log.Printf("boot scan skipped: project needs setup")
	} else if c.ModelCrawling != "" && hasEmbedding {
		bootCtx, cancelBoot := context.WithCancel(context.Background())
		if err := eng.BootScan(bootCtx); err != nil {
			log.Printf("boot scan encountered errors: %v", err)
		}
		cancelBoot()
	} else {
		log.Printf("boot scan skipped: models not configured")
	}

	eng.UnlockSystemTask()

	if err := eng.StartWatcher(context.Background()); err != nil {
		log.Printf("fsnotify watcher encountered errors: %v", err)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		if err != nil {
			log.Printf("HTTP server: %v", err)
		}
	case <-sigCtx.Done():
	}
	eng.Stop()
	for deadline := time.Now().Add(4 * time.Second); eng.Status().Running && time.Now().Before(deadline); {
		time.Sleep(25 * time.Millisecond)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if st.HasActiveSession() && st.PersistenceStatus().Dirty {
		if err := st.Save("shutdown"); err != nil {
			log.Printf("save state on shutdown: %v", err)
		}
	}
}
