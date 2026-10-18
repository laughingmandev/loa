package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

func TestWatcher_MarkStale(t *testing.T) {
	root := t.TempDir()
	st := state.NewStore(root, state.NewAgentState())
	cfgv := config.Default()
	cfg := config.NewManager(cfgv)
	idx := index.New(root)

	memStore := memory.NewStore(root, 336.0, 0.03)
	eng := New(root, st, memStore, cfg, &scriptedLLM{}, tools.New(root, nil, cfg.Get, nil), idx)
	// Simulate an active task running so watcher defers crawling and keeps staleFiles marked.
	eng.running = true

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := eng.StartWatcher(ctx); err != nil {
		t.Fatal(err)
	}

	// create a file
	filePath := filepath.Join(root, "test.go")
	if err := os.WriteFile(filePath, []byte("package main"), 0644); err != nil {
		t.Fatal(err)
	}

	// give watcher some time to debounce
	time.Sleep(800 * time.Millisecond)

	if !eng.hasStaleFiles() {
		t.Fatal("expected test.go to be marked stale after fsnotify event while engine is running")
	}
}

func TestWatcher_RepairStaleFiles(t *testing.T) {
	root := t.TempDir()

	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	st := state.NewStore(root, state.NewAgentState())
	cfgv := config.Default()
	cfg := config.NewManager(cfgv)
	idx := index.New(root)
	ctx := context.Background()

	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	memStore := memory.NewStore(root, 336.0, 0.03)
	err := memStore.AddMemory(state.MemoryItem{
		Kind:     state.MemoryStructural,
		Anchors:  []string{"main.go"},
		FileHash: "deadbeef",
		Text:     "Old frame",
	})
	if err != nil {
		t.Fatal(err)
	}

	fake := &scriptedLLM{json: []string{
		`{"frame":"New frame"}`,
		`{"index_keys":["main"],"reason":"main"}`,
	}}
	eng := New(root, st, memStore, cfg, fake, tools.New(root, nil, cfg.Get, nil), idx)

	// artificially mark stale
	eng.markStale(filePath)

	if err := eng.repairStaleFiles(ctx); err != nil {
		t.Fatal(err)
	}

	if eng.hasStaleFiles() {
		t.Fatal("expected stale files to be cleared")
	}

	items := memStore.Items()
	found := false
	for _, item := range items {
		if len(item.Anchors) > 0 && item.Anchors[0] == "main.go" {
			found = true
			if item.Text != "New frame" {
				t.Fatalf("expected New frame, got %s", item.Text)
			}
		}
	}
	if !found {
		t.Fatal("main.go not found in mem store")
	}
}

func TestLazyStaleRepair_RetrieveMemoryFlushesStaleFilesJustInTime(t *testing.T) {
	root := t.TempDir()
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, []byte("package main\nfunc main() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}

	st := state.NewStore(root, state.NewAgentState())
	cfgv := config.Default()
	cfgv.EmbeddingEngine = "openai"
	cfgv.EmbeddingModel = "fake-embed"
	cfg := config.NewManager(cfgv)
	idx := index.New(root)
	ctx := context.Background()

	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	memStore := memory.NewStore(root, 336.0, 0.03)
	if err := memStore.AddMemory(state.MemoryItem{
		Kind:      state.MemoryStructural,
		Anchors:   []string{"main.go"},
		FileHash:  "deadbeef",
		Text:      "Stale frame before edit",
		Embedding: []float32{1, 0},
	}); err != nil {
		t.Fatal(err)
	}

	fake := &scriptedLLM{json: []string{
		`{"frame":"Updated frame from JIT"}`,
		`{"index_keys":["main","jit"],"reason":"updated"}`,
	}}
	eng := New(root, st, memStore, cfg, fake, tools.New(root, idx, cfg.Get, nil), idx)

	eng.markStale(filePath)
	if !eng.hasStaleFiles() {
		t.Fatal("expected main.go to be marked stale")
	}

	// Calling retrieveMemoryRound must flush stale files before searching memStore.
	got, err := eng.retrieveMemoryRound(ctx, "main", nil, nil, 0)
	if err != nil {
		t.Fatalf("retrieveMemoryRound failed: %v", err)
	}
	if eng.hasStaleFiles() {
		t.Fatal("expected retrieveMemoryRound pre-hook to clear stale files")
	}
	if !strings.Contains(got, "Updated frame from JIT") || strings.Contains(got, "Stale frame before edit") {
		t.Fatalf("expected JIT-refreshed frame in memory results, got:\n%s", got)
	}
}

func TestLazyStaleRepair_RepeatedWritesSkipIntermediateCrawlsAndFlushOnceAtFinish(t *testing.T) {
	root := t.TempDir()
	st := state.NewStore(root, state.NewAgentState())
	if _, err := st.CreateSession("Lazy repair test"); err != nil {
		t.Fatal(err)
	}
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingEngine = "openai"
	cfgv.EmbeddingModel = "fake-embed"
	cfgv.Permissions.Mode = config.PermissionAllowAll
	cfg := config.NewManager(cfgv)
	idx := index.New(root)
	ctx := context.Background()

	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}

	memStore := memory.NewStore(root, 336.0, 0.03)
	tm := tools.New(root, idx, cfg.Get, nil)

	step := state.PlanStep{
		ID:             10,
		Title:          "Create and patch main.go",
		Goal:           "write main.go, read it, patch it twice, read it again, and complete",
		Mode:           state.StepModify,
		Status:         state.StepRunning,
		ExpectedResult: "main.go contains v3",
	}
	st.SetActiveTask(&state.TaskState{
		ID:             1,
		Goal:           "build main.go",
		MayModify:      true,
		ComplexityMode: "Planned",
		Status:         state.TaskRunning,
		Plan:           state.Plan{Version: 1, Steps: []state.PlanStep{step}},
	})

	fake := &scriptedLLM{json: []string{
		// 1. Step-start MemoryStep keyword extraction
		`{"index_keys":["main","patch"]}`,
		// 2. Loop 0: write_file v1 (with evaluate_after: false to stay in blind execution)
		`{"action":"tool","reason":"write v1","evaluate_after":false,"tool":{"kind":"write_file","input":{"path":"main.go","content":"package main\nconst V = 1\n"}}}`,
		`{"hypothesis":"v1 written","expected_effect":"file created"}`,
		// 3. Loop 1: read_file main.go
		`{"action":"tool","reason":"read v1","tool":{"kind":"read_file","input":{"path":"main.go"}}}`,
		// 4. Loop 2: patch_file v1 -> v2
		`{"action":"tool","reason":"patch v2","evaluate_after":false,"tool":{"kind":"patch_file","input":{"path":"main.go","old":"const V = 1","new":"const V = 2","replace_all":false}}}`,
		`{"hypothesis":"v2 patched","expected_effect":"V is 2"}`,
		// 5. Loop 3: patch_file v2 -> v3
		`{"action":"tool","reason":"patch v3","evaluate_after":false,"tool":{"kind":"patch_file","input":{"path":"main.go","old":"const V = 2","new":"const V = 3","replace_all":false}}}`,
		`{"hypothesis":"v3 patched","expected_effect":"V is 3"}`,
		// 6. Loop 4: read_file main.go again (must succeed because invalidatedPaths evicted the read cache!)
		`{"action":"tool","reason":"read v3","tool":{"kind":"read_file","input":{"path":"main.go"}}}`,
		// 7. Loop 5: step_complete
		`{"action":"step_complete","reason":"done","completion_summary":"updated main.go to V = 3"}`,
		// 8. EvaluateStepCompletion + completeStep ceremony
		`{"status":"success","reason":"V is 3","assumptions":[],"suggested_recovery":"","verification_need":""}`,
		`{"artifact_payload":""}`,
		`{"summary":"Created and patched main.go to V = 3.","facts_learned":["V is 3"],"decisions":[]}`,
		`{"facts":["V is 3"],"decisions":[],"constraints":[],"open_issues":[]}`,
		`{"index_keys":["main","v3"]}`,
	}}

	eng := New(root, st, memStore, cfg, fake, tm, idx)
	outcome, _, err := eng.executeStep(ctx, 10)
	if err != nil {
		t.Fatalf("executeStep failed: %v", err)
	}
	if outcome != "done" {
		t.Fatalf("expected outcome=done, got %q", outcome)
	}

	// Crucial assertion 1: Despite 1 write_file + 2 patch_file calls on main.go, ZERO NarrativeFrame crawls
	// ran during executeStep because memory was not queried mid-step.
	if got := eng.Status().Stats.InferenceByPrimitive["NarrativeFrame"]; got != 0 {
		t.Fatalf("expected 0 NarrativeFrame calls during executeStep, got %d", got)
	}
	if !eng.hasStaleFiles() {
		t.Fatal("expected main.go to remain marked stale until retrieval or task completion")
	}

	// Now simulate task completion (Final Synthesis already completed) and verify finishTask flushes stale files once.
	snap := st.Snapshot()
	artDir := filepath.Join(root, ".loa", "artifacts", snap.SessionID, "task-1")
	if err := os.MkdirAll(artDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "final_response.md"), []byte("Finished main.go V=3."), 0644); err != nil {
		t.Fatal(err)
	}
	st.UpdateActiveTask(func(t *state.TaskState) {
		t.Plan.Steps = append(t.Plan.Steps, state.PlanStep{
			ID:     11,
			Title:  "Final Synthesis",
			Mode:   state.StepSynthesize,
			Status: state.StepCompleted,
		})
	})

	fake.json = append(fake.json,
		// SummarizeTask + ExtractKeywords
		`{"summary":"Implemented main.go with V=3."}`,
		`{"index_keys":["main","v3"]}`,
		// ConsolidateContext
		`{"items":[]}`,
		// MemoryTask ExtractKeywords
		`{"index_keys":["main","task"]}`,
		// Single post-task crawler run (NarrativeFrame + ExtractKeywords)
		`{"frame":"main.go defines V = 3"}`,
		`{"index_keys":["main","V"]}`,
	)

	if err := eng.finishTask(ctx, primitives.FinalCheck{Complete: true}); err != nil {
		t.Fatalf("finishTask failed: %v", err)
	}

	if eng.hasStaleFiles() {
		t.Fatal("expected finishTask to flush all stale files")
	}
	if got := eng.Status().Stats.InferenceByPrimitive["NarrativeFrame"]; got != 1 {
		t.Fatalf("expected exactly 1 NarrativeFrame crawl at task completion (instead of 3), got %d", got)
	}

	items := memStore.Items()
	found := false
	for _, it := range items {
		if it.Kind == state.MemoryStructural && len(it.Anchors) > 0 && it.Anchors[0] == "main.go" {
			found = true
			if it.Text != "main.go defines V = 3" {
				t.Fatalf("unexpected structural memory text: %q", it.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected main.go structural memory in memStore after finishTask flush")
	}
}

func TestLazyStaleRepair_FastModeFlushesStaleFilesOnCompletion(t *testing.T) {
	root := t.TempDir()
	st := state.NewStore(root, state.NewAgentState())
	if _, err := st.CreateSession("Fast lazy repair test"); err != nil {
		t.Fatal(err)
	}
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingEngine = "openai"
	cfgv.EmbeddingModel = "fake-embed"
	cfgv.Permissions.Mode = config.PermissionAllowAll
	cfg := config.NewManager(cfgv)
	idx := index.New(root)
	ctx := context.Background()

	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}

	memStore := memory.NewStore(root, 336.0, 0.03)
	tm := tools.New(root, idx, cfg.Get, nil)

	step := state.PlanStep{
		ID:     20,
		Title:  "Execute Fast Track",
		Goal:   "write and patch fast.go",
		Mode:   state.StepModify,
		Status: state.StepRunning,
	}
	st.SetActiveTask(&state.TaskState{
		ID:             2,
		Goal:           "write and patch fast.go",
		MayModify:      true,
		ComplexityMode: "Fast",
		Status:         state.TaskRunning,
		Plan:           state.Plan{Version: 1, Steps: []state.PlanStep{step}},
	})

	fake := &scriptedLLM{json: []string{
		// 1. Step-start MemoryStep keyword extraction
		`{"index_keys":["fast","patch"]}`,
		// 2. Loop 0: write_file fast.go
		`{"action":"tool","reason":"write fast.go","evaluate_after":false,"tool":{"kind":"write_file","input":{"path":"fast.go","content":"package main\nconst Fast = 1\n"}}}`,
		`{"hypothesis":"fast.go written","expected_effect":"file created"}`,
		// 3. Loop 1: patch_file fast.go
		`{"action":"tool","reason":"patch fast.go","evaluate_after":false,"tool":{"kind":"patch_file","input":{"path":"fast.go","old":"const Fast = 1","new":"const Fast = 2","replace_all":false}}}`,
		`{"hypothesis":"fast.go patched","expected_effect":"Fast is 2"}`,
		// 4. Loop 2: step_complete (triggers fast_complete)
		`{"action":"step_complete","reason":"done","completion_summary":"updated fast.go"}`,
		// 5. SynthesizeTask
		`{"response":"Created and patched fast.go to Fast = 2."}`,
		// 6. SummarizeTask + ExtractKeywords
		`{"summary":"Updated fast.go in Fast mode."}`,
		`{"index_keys":["fast","mode"]}`,
		// 7. MemoryTask ExtractKeywords
		`{"index_keys":["fast","task"]}`,
		// 8. Single post-task crawler run (NarrativeFrame + ExtractKeywords)
		`{"frame":"fast.go defines Fast = 2"}`,
		`{"index_keys":["fast","Fast"]}`,
	}}

	eng := New(root, st, memStore, cfg, fake, tm, idx)
	outcome, _, err := eng.executeStep(ctx, 20)
	if err != nil {
		t.Fatalf("executeStep (Fast) failed: %v", err)
	}
	if outcome != "fast_complete" {
		t.Fatalf("expected outcome=fast_complete, got %q", outcome)
	}
	if eng.hasStaleFiles() {
		t.Fatal("expected fast_complete to flush all stale files")
	}
	if got := eng.Status().Stats.InferenceByPrimitive["NarrativeFrame"]; got != 1 {
		t.Fatalf("expected exactly 1 NarrativeFrame crawl at fast_complete (instead of 2), got %d", got)
	}

	items := memStore.Items()
	found := false
	for _, it := range items {
		if it.Kind == state.MemoryStructural && len(it.Anchors) > 0 && it.Anchors[0] == "fast.go" {
			found = true
			if it.Text != "fast.go defines Fast = 2" {
				t.Fatalf("unexpected structural memory text: %q", it.Text)
			}
		}
	}
	if !found {
		t.Fatal("expected fast.go structural memory in memStore after fast_complete flush")
	}
}

func TestSymbolLessFiles_IndexedAndSurviveBootScan(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	// 1. Write a symbol-less markdown file (no '#' headings), a plain text file,
	// an empty file, a binary file, and a .loaignore-excluded file.
	if err := os.WriteFile(filepath.Join(root, ".loaignore"), []byte("ignored.txt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "anagentsdream.md"), []byte("In circuits deep where tokens flow,\nA quiet context starts to grow.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("should be ignored by .loaignore\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), []byte("   \n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary.bin"), []byte{0x00, 0x01, 0x02, 0xff}, 0644); err != nil {
		t.Fatal(err)
	}

	idx := index.New(root)
	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	snap := idx.Snapshot()
	if _, ok := snap["anagentsdream.md"]; !ok {
		t.Fatalf("expected symbol-less text file anagentsdream.md in index Snapshot, got %v", idx.Files())
	}
	if _, ok := snap["ignored.txt"]; ok {
		t.Fatal("did not expect .loaignore file ignored.txt in index Snapshot")
	}
	if _, ok := snap["binary.bin"]; ok {
		t.Fatal("did not expect binary file binary.bin in index Snapshot")
	}
	if _, ok := snap[".loaignore"]; ok {
		t.Fatal("did not expect hidden file .loaignore in index Snapshot")
	}

	st := state.NewStore(root, state.NewAgentState())
	cfgv := config.Default()
	cfgv.EmbeddingEngine = "openai"
	cfgv.EmbeddingModel = "fake-embed"
	cfg := config.NewManager(cfgv)
	memStore := memory.NewStore(root, 336.0, 0.03)

	fake := &scriptedLLM{json: []string{
		`{"frame":"anagentsdream.md is a poem about an AI agent dreaming of clean context"}`,
		`{"index_keys":["poem","agent","dream"]}`,
	}}
	eng := New(root, st, memStore, cfg, fake, tools.New(root, idx, cfg.Get, nil), idx)

	if err := eng.RunCrawler(ctx); err != nil {
		t.Fatalf("RunCrawler failed: %v", err)
	}

	// Only anagentsdream.md should have triggered NarrativeFrame (empty.txt is skipped).
	if got := eng.Status().Stats.InferenceByPrimitive["NarrativeFrame"]; got != 1 {
		t.Fatalf("expected 1 NarrativeFrame call (for anagentsdream.md, skipping empty.txt), got %d", got)
	}

	// Verify BootScan keeps anagentsdream.md's structural memory when file is unchanged.
	if err := eng.BootScan(ctx); err != nil {
		t.Fatalf("BootScan failed: %v", err)
	}

	found := false
	for _, it := range memStore.Items() {
		if it.Kind == state.MemoryStructural && len(it.Anchors) > 0 && it.Anchors[0] == "anagentsdream.md" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected anagentsdream.md structural memory to survive BootScan")
	}
}

func TestWatcher_IdleAutoCrawl(t *testing.T) {
	root := t.TempDir()
	st := state.NewStore(root, state.NewAgentState())
	cfgv := config.Default()
	cfgv.FSWatcherDebounceMs = 100
	cfgv.EmbeddingEngine = "openai"
	cfgv.EmbeddingModel = "fake-embed"
	cfg := config.NewManager(cfgv)
	idx := index.New(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := idx.Probe(ctx); err != nil {
		t.Skip("ctags not available")
	}
	if err := idx.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}

	memStore := memory.NewStore(root, 336.0, 0.03)
	fake := &scriptedLLM{json: []string{
		`{"frame":"somefile.txt contains idle watcher test notes"}`,
		`{"index_keys":["somefile","idle","watcher"]}`,
	}}
	eng := New(root, st, memStore, cfg, fake, tools.New(root, idx, cfg.Get, nil), idx)
	// Engine is idle (eng.running == false && eng.needsProjectSetup == false)

	if err := eng.StartWatcher(ctx); err != nil {
		t.Fatal(err)
	}

	// Create a new file on disk while LOA is idling
	filePath := filepath.Join(root, "somefile.txt")
	if err := os.WriteFile(filePath, []byte("hello from disk while loa is idle\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Wait for debounce + background idle crawl to complete
	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		for _, it := range memStore.Items() {
			if it.Kind == state.MemoryStructural && len(it.Anchors) > 0 && it.Anchors[0] == "somefile.txt" {
				found = true
				break
			}
		}
		if found && !eng.hasStaleFiles() && !eng.Status().Running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !found {
		t.Fatal("expected idle watcher to automatically crawl somefile.txt into memStore without waiting for next prompt")
	}
	if eng.hasStaleFiles() {
		t.Fatal("expected staleFiles to be empty after idle auto-crawl")
	}
}
