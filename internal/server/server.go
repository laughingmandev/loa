package server

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/laughingmandev/loa/internal/agent"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/embedder"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/version"
	"strconv"
)

//go:embed web/*
var webAssets embed.FS

type Server struct {
	root  string
	addr  string
	store *state.Store
	cfg   *config.Manager
	llm   llm.Client
	agent *agent.Engine
	index *index.Index
	token string
	http  *http.Server
}

func New(root, addr string, st *state.Store, cfg *config.Manager, lc llm.Client, eng *agent.Engine, idx *index.Index) (*Server, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s := &Server{root: root, addr: addr, store: st, cfg: cfg, llm: lc, agent: eng, index: idx, token: hex.EncodeToString(b)}
	s.http = &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: time.Duration(cfg.Get().ServerReadHeaderTimeoutSeconds) * time.Second}
	return s, nil
}

func (s *Server) Run() error {
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("GET /api/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /api/models", s.handleModels)
	mux.HandleFunc("GET /api/models/local", s.handleLocalModels)
	mux.HandleFunc("POST /api/message", s.writeGuard(s.handleMessage))
	mux.HandleFunc("POST /api/approval", s.writeGuard(s.handleApproval))
	mux.HandleFunc("POST /api/plan/approval", s.writeGuard(s.handlePlanApproval))
	mux.HandleFunc("POST /api/stop", s.writeGuard(s.handleStop))
	mux.HandleFunc("POST /api/pause", s.writeGuard(s.handlePause))
	mux.HandleFunc("POST /api/resume", s.writeGuard(s.handleResume))
	mux.HandleFunc("POST /api/quit", s.writeGuard(s.handleQuit))
	mux.HandleFunc("POST /api/save", s.writeGuard(s.handleSave))
	mux.HandleFunc("POST /api/session", s.writeGuard(s.handleCreateSession))
	mux.HandleFunc("POST /api/session/modes", s.writeGuard(s.handleSessionModes))
	mux.HandleFunc("POST /api/session/switch", s.writeGuard(s.handleSwitchSession))
	mux.HandleFunc("POST /api/reindex", s.writeGuard(s.handleReindex))
	mux.HandleFunc("POST /api/debug-dump", s.writeGuard(s.handleDebugDump))
	mux.HandleFunc("POST /api/config", s.writeGuard(s.handleConfig))
	mux.HandleFunc("GET /api/config/default", s.handleConfigDefault)
	mux.HandleFunc("POST /api/loaignore", s.writeGuard(s.handleLoaignore))
	mux.HandleFunc("POST /api/setup/project", s.writeGuard(s.handleSetupProject))
	mux.HandleFunc("POST /api/test-llm", s.writeGuard(s.handleTestLLM))
	mux.HandleFunc("GET /api/memory/tree", s.handleMemoryTree)
	mux.HandleFunc("GET /api/memory/file", s.handleMemoryFile)
	mux.HandleFunc("POST /api/memory/frame", s.writeGuard(s.handleMemoryFrame))
	mux.HandleFunc("POST /api/memory/edit", s.writeGuard(s.handleMemoryEdit))
	mux.HandleFunc("POST /api/memory/delete", s.writeGuard(s.handleMemoryDelete))
	mux.HandleFunc("POST /api/memory/purge", s.writeGuard(s.handleMemoryPurge))
	mux.HandleFunc("POST /api/upload", s.writeGuard(s.handleUpload))
	mux.HandleFunc("POST /api/upload/delete", s.writeGuard(s.handleUploadDelete))
	mux.HandleFunc("POST /api/upload/readonly", s.writeGuard(s.handleUploadReadOnly))
	mux.HandleFunc("GET /api/upload/{id}/download", s.handleUploadDownload)
	mux.HandleFunc("POST /api/attach", s.writeGuard(s.handleAttach))
	mux.HandleFunc("POST /api/attach/delete", s.writeGuard(s.handleAttachDelete))
	mux.HandleFunc("GET /api/artifact", s.handleArtifactContent)
	sub, _ := fs.Sub(webAssets, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline' 'unsafe-eval' blob:; worker-src 'self' blob:; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) writeGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Loa-Token") != s.token {
			writeErr(w, http.StatusForbidden, "invalid local session token")
			return
		}
		if ct := r.Header.Get("Content-Type"); r.ContentLength > 0 && !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "multipart/form-data") {
			writeErr(w, http.StatusUnsupportedMediaType, "application/json or multipart/form-data required")
			return
		}
		next(w, r)
	}
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"token": s.token, "project_root": s.root, "version": version.String})
}
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	v := r.URL.Query().Get("v")
	if v != "" {
		if reqSeq, err := strconv.ParseUint(v, 10, 64); err == nil && reqSeq == snap.Sequence {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	lastLogIDStr := r.URL.Query().Get("last_log_id")
	lastLogSession := r.URL.Query().Get("last_log_session")
	if lastLogIDStr != "" && lastLogSession == snap.SessionID {
		if lastLogID, err := strconv.ParseUint(lastLogIDStr, 10, 64); err == nil {
			var filtered []state.ExecutionLogEntry
			for _, l := range snap.ExecutionLog {
				if l.ID > lastLogID {
					filtered = append(filtered, l)
				}
			}
			snap.ExecutionLog = filtered
		}
	}
	ignoreData, _ := os.ReadFile(filepath.Join(s.root, ".loaignore"))
	mem := s.agent.MemStore()
	sessionMem := s.agent.SessionMemStore()
	var sessionMemories []state.MemoryItem
	var sessionSummaries []state.TaskSummary
	if sessionMem != nil {
		sessionMemories = sessionMem.SessionMemories()
		sessionSummaries = sessionMem.Summaries()
	} else {
		sessionSummaries = mem.Summaries()
	}
	artifacts := s.getArtifacts()
	writeJSON(w, http.StatusOK, map[string]any{"state": snap, "runtime": s.agent.Status(), "config": s.cfg.Get(), "sessions": s.store.SessionStatus(), "persistence": s.store.PersistenceStatus(), "loaignore": string(ignoreData), "memory": sessionMemories, "task_summaries": sessionSummaries, "artifacts": artifacts})
}
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.llm.Models(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleLocalModels(w http.ResponseWriter, r *http.Request) {
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	modelsDir := filepath.Join(home, ".loa", "embedding-models")
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"models": []string{}})
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var models []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".gguf") {
			// Save paths using ~/ prefix so they are portable across different users/hosts
			portablePath := filepath.Join("~", ".loa", "embedding-models", entry.Name())
			models = append(models, portablePath)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !s.store.HasActiveSession() {
		writeErr(w, http.StatusConflict, "create a session before sending messages")
		return
	}
	if err := s.agent.SubmitMessage(in.Text); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, agent.ErrBusy) {
			code = http.StatusConflict
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ToolCallID uint64 `json:"tool_call_id"`
		Approved   bool   `json:"approved"`
		AllowRun   bool   `json:"allow_run"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.agent.Approve(in.ToolCallID, in.Approved, in.AllowRun); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
func (s *Server) handlePlanApproval(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Approved bool `json:"approved"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.agent.ApprovePlan(r.Context(), in.Approved); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.agent.Stop()
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.agent.Pause()
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.Resume(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}
func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	s.agent.Pause()
	s.store.Save("manual")
	writeJSON(w, http.StatusOK, map[string]string{"status": "shutting_down"})
	
	// Wait a brief moment to ensure the HTTP response is sent, then exit
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.Exit(0)
	}()
}
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Save("manual"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := s.store.SessionStatus()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": status.ActiveTitle, "persistence": s.store.PersistenceStatus()})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "stop the active agent run before creating a session")
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &in) {
		return
	}
	savedCurrent := s.store.HasActiveSession() && s.store.PersistenceStatus().Dirty
	info, err := s.store.CreateSession(in.Title)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent.ResetSessionView()
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "saved_current": savedCurrent, "session": info, "sessions": s.store.SessionStatus(), "persistence": s.store.PersistenceStatus()})
}

func (s *Server) handleSessionModes(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "cannot change session modes while agent is running")
		return
	}
	var in struct {
		ComplexityMode string `json:"complexity_mode"`
		ReadOnly       bool   `json:"read_only"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.store.SetSessionModes(in.ComplexityMode, in.ReadOnly)
	
	if err := s.store.Save("modes_update"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSwitchSession(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "stop the active agent run before switching sessions")
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	saved, info, err := s.store.SwitchSession(in.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.agent.ResetSessionView()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_current": saved, "session": info, "sessions": s.store.SessionStatus(), "persistence": s.store.PersistenceStatus()})
}
func (s *Server) handleReindex(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "stop the active agent run before reindexing")
		return
	}
	// Phase 3.4: Remove the HTTP context timeout and invoke Hard Reset
	// This blocks the API until the crawler completes a full wipe and rebuild of the memory graph.
	if err := s.agent.ReindexHardReset(context.Background()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
func (s *Server) handleDebugDump(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IncludeEmbeddings bool `json:"include_embeddings"`
		FrontendLogs      any  `json:"frontend_logs"`
	}
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	b, err := s.agent.DebugDump(in.IncludeEmbeddings)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	if in.FrontendLogs != nil {
		var dumpData map[string]any
		if err := json.Unmarshal(b, &dumpData); err == nil {
			dumpData["frontend_logs"] = in.FrontendLogs
			if nb, err := json.MarshalIndent(dumpData, "", "  "); err == nil {
				b = nb
			}
		}
	}
	name := "loa-debug-" + time.Now().Format("20060102-150405") + ".json"
	dir := state.DebugDir(s.root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file": filepath.ToSlash(filepath.Join(state.RuntimeDirname, state.DebugDirname, name))})
}
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "stop the active agent run before changing settings")
		return
	}
	var in config.Config
	if !decode(w, r, &in) {
		return
	}
	scope := r.URL.Query().Get("scope")
	var err error
	switch scope {
	case "global":
		err = config.SaveGlobal(in)
	case "project", "":
		err = config.SaveProject(s.root, in)
	default:
		writeErr(w, http.StatusBadRequest, "scope must be project or global")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cfg.Set(in)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scope": func() string {
		if scope == "" {
			return "project"
		}
		return scope
	}()})
}

func (s *Server) handleConfigDefault(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, config.Default())
}

func (s *Server) handleLoaignore(w http.ResponseWriter, r *http.Request) {
	if s.agent.Status().Running {
		writeErr(w, http.StatusConflict, "stop the active agent run before changing settings")
		return
	}
	var in struct {
		IgnoreList string `json:"ignore_list"`
	}
	if !decode(w, r, &in) {
		return
	}
	ignorePath := filepath.Join(s.root, ".loaignore")
	if err := os.WriteFile(ignorePath, []byte(in.IgnoreList), 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to write .loaignore: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func (s *Server) handleSetupProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IgnoreList          string `json:"ignore_list"`
		Purpose             string `json:"purpose"`
		InitialSessionTitle string `json:"initial_session_title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ignorePath := filepath.Join(s.root, ".loaignore")
	if err := os.WriteFile(ignorePath, []byte(req.IgnoreList), 0644); err != nil {
		writeErr(w, http.StatusInternalServerError, "Failed to write .loaignore: "+err.Error())
		return
	}



	s.agent.SetNeedsProjectSetup(false)

	title := req.InitialSessionTitle
	if title == "" {
		title = "Initial Session"
	}
	_, err := s.store.CreateSession(title)
	if err != nil {
		log.Printf("failed to create initial session: %v", err)
	}

	go func(purpose string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.agent.LockSystemTask("indexing")
		if purpose != "" {
			s.agent.AddGlobalConcept(ctx, purpose)
		}
		if err := s.agent.BootScan(ctx); err != nil {
			log.Printf("boot scan after setup failed: %v", err)
		}
		s.agent.UnlockSystemTask()
	}(req.Purpose)

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMemoryTree(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		writeJSON(w, http.StatusOK, []string{})
		return
	}
	files := s.index.Files()
	writeJSON(w, http.StatusOK, files)
}

func (s *Server) handleMemoryFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}

	var syms []index.Symbol
	var frames []state.MemoryItem

	if path == "__GLOBAL__" {
		if s.store != nil {
			memStore := memory.NewStore(s.root, s.cfg.Get().MemoryLTSDecayHalfLifeHours, s.cfg.Get().MemoryLTSMaxWeight)
			if err := memStore.Load(); err == nil {
				frames = memStore.GlobalMemories()
			}
		}
	} else {
		if s.index != nil {
			syms = s.index.SymbolsForFile(path)
		}
		if s.store != nil {
			memStore := memory.NewStore(s.root, s.cfg.Get().MemoryLTSDecayHalfLifeHours, s.cfg.Get().MemoryLTSMaxWeight)
			if err := memStore.Load(); err == nil {
				frames = memStore.FileMemories(path)
			}
		}
	}

	if syms == nil {
		syms = []index.Symbol{}
	}
	if frames == nil {
		frames = []state.MemoryItem{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"symbols": syms,
		"frames":  frames,
	})
}

func (s *Server) handleMemoryFrame(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      uint64   `json:"id"`
		Kind    string   `json:"kind"`
		Text    string   `json:"text"`
		Anchors []string `json:"anchors"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text required")
		return
	}

	memStore := memory.NewStore(s.root, s.cfg.Get().MemoryLTSDecayHalfLifeHours, s.cfg.Get().MemoryLTSMaxWeight)
	if err := memStore.Load(); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load store")
		return
	}

	var ext struct {
		IndexKeys []string `json:"index_keys"`
	}
	sysPrompt := `Analyze the provided narrative frame or memory text. Extract a concise list of the most important structural and semantic keywords, concept names, identifiers, or file paths that represent its core meaning. These keys will be used for vector embedding and retrieval.`
	_, err := s.llm.ChatJSON(r.Context(), s.cfg.Get().ModelCrawling, sysPrompt, func(repairCost int) (string, error) { return req.Text, nil }, &ext, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to extract keywords: "+err.Error())
		return
	}

	embedText := req.Text
	if len(ext.IndexKeys) > 0 {
		embedText = strings.Join(ext.IndexKeys, " ")
	}

	emb, err := s.llm.Embed(r.Context(), embedText)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to embed text: "+err.Error())
		return
	}

	if req.ID > 0 {
		if err := memStore.UpdateMemory(req.ID, req.Text, emb, ext.IndexKeys); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to update memory")
			return
		}
	} else {
		item := state.MemoryItem{
			Kind:      state.MemoryKind(req.Kind),
			Text:      req.Text,
			Embedding: emb,
			IndexKeys: ext.IndexKeys,
			Anchors:   req.Anchors,
		}
		if err := memStore.AddMemory(item); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to add memory")
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) URL() string { return fmt.Sprintf("http://%s", s.addr) }



func (s *Server) getArtifacts() map[uint64][]string {
	out := make(map[uint64][]string)
	state := s.store.Snapshot()
	if state.SessionID == "" {
		return out
	}
	dir := filepath.Join(s.root, ".loa", "artifacts", state.SessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "task-") {
			taskIDStr := strings.TrimPrefix(e.Name(), "task-")
			if id, err := strconv.ParseUint(taskIDStr, 10, 64); err == nil {
				taskDir := filepath.Join(dir, e.Name())
				files, _ := os.ReadDir(taskDir)
				var names []string
				for _, f := range files {
					if !f.IsDir() {
						names = append(names, f.Name())
					}
				}
				if len(names) > 0 {
					out[id] = names
				}
			}
		}
	}
	return out
}

func (s *Server) handleArtifactContent(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	taskIDStr := r.URL.Query().Get("task_id")
	if name == "" || taskIDStr == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		http.Error(w, "invalid artifact name or task_id", http.StatusBadRequest)
		return
	}
	state := s.store.Snapshot()
	if state.SessionID == "" {
		http.Error(w, "no active session", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.root, ".loa", "artifacts", state.SessionID, fmt.Sprintf("task-%s", taskIDStr), name)
	b, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func (s *Server) handleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID uint64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	if sessionMem := s.agent.SessionMemStore(); sessionMem != nil {
		_ = sessionMem.DeleteMemory(req.ID)
	}

	memStore := memory.NewStore(s.root, s.cfg.Get().MemoryLTSDecayHalfLifeHours, s.cfg.Get().MemoryLTSMaxWeight)
	if err := memStore.Load(); err == nil {
		_ = memStore.DeleteMemory(req.ID)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMemoryEdit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   uint64 `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	emb, err := s.llm.Embed(r.Context(), req.Text)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to embed text: "+err.Error())
		return
	}

	if sessionMem := s.agent.SessionMemStore(); sessionMem != nil {
		_ = sessionMem.UpdateMemory(req.ID, req.Text, emb, nil)
	}

	memStore := memory.NewStore(s.root, s.cfg.Get().MemoryLTSDecayHalfLifeHours, s.cfg.Get().MemoryLTSMaxWeight)
	if err := memStore.Load(); err == nil {
		_ = memStore.UpdateMemory(req.ID, req.Text, emb, nil)
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleMemoryPurge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		ID    uint64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	if req.Type == "session_memory" {
		if sessionMem := s.agent.SessionMemStore(); sessionMem != nil {
			_ = sessionMem.DeleteMemory(req.ID)
		}
	} else if req.Type == "extra" {
		s.agent.PurgeActiveTaskExtra(req.Index)
	} else {
		writeErr(w, http.StatusBadRequest, "invalid type")
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleTestLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL                    string `json:"ollama_url"`
		Key                    string `json:"api_key"`
		ChatModel              string `json:"chat_model"`
		EmbeddingEngine        string `json:"embedding_engine"`
		EmbeddingModel         string `json:"embedding_model"`
		LocalEmbeddingModelPath string `json:"local_embedding_model_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request")
		return
	}

	testCfg := config.Default()
	testCfg.OllamaURL = req.URL
	testCfg.APIKey = req.Key
	testCfg.EmbeddingModel = req.EmbeddingModel
	testClient := llm.New(func() config.Config { return testCfg })

	if req.ChatModel == "" {
		writeErr(w, http.StatusBadRequest, "Inference model is required for testing")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, err := testClient.ChatText(ctx, req.ChatModel, "You are a test bot. Respond with 'ok'.", "Respond with 'ok'")
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("Chat model test failed for '%s': %v", req.ChatModel, err))
		return
	}

	if req.EmbeddingEngine == "local" {
		if req.LocalEmbeddingModelPath == "" {
			writeErr(w, http.StatusBadRequest, "Local embedding model path is required when using Local CPU engine")
			return
		}
		emb, err := embedder.NewLocalEmbedder(req.LocalEmbeddingModelPath)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("Failed to load local embedding model: %v", err))
			return
		}
		defer emb.Close()
		
		vec, err := emb.Embed("test embedding")
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("Failed to generate embedding with local model: %v", err))
			return
		}
		if len(vec) == 0 {
			writeErr(w, http.StatusBadRequest, "Local embedding model returned 0 vector dimensions")
			return
		}
	} else if req.EmbeddingModel != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		vec, err := testClient.Embed(ctx, "test embedding")
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("Embedding model test failed for '%s'. Are you sure it's an embedding model? Error: %v", req.EmbeddingModel, err))
			return
		}
		if len(vec) == 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("Embedding model '%s' returned 0 vector dimensions.", req.EmbeddingModel))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(32 << 20)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to parse multipart form")
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "no files provided")
		return
	}

	var results []state.UploadState
	sessionID := s.store.Snapshot().SessionID
	uploadDir := filepath.Join(s.root, ".loa", "uploads", sessionID)
	os.MkdirAll(uploadDir, 0755)

	for _, fileHeader := range files {
		file, err := fileHeader.Open()
		if err != nil {
			continue
		}
		
		id := fmt.Sprintf("up_%d", time.Now().UnixNano())
		safeFilename := filepath.Base(fileHeader.Filename)
		destPath := filepath.Join(uploadDir, fmt.Sprintf("%s_%s", id, safeFilename))
		
		dest, err := os.Create(destPath)
		if err != nil {
			file.Close()
			continue
		}
		
		io.Copy(dest, file)
		dest.Close()
		file.Close()

		upload := state.UploadState{
			ID:           id,
			Filename:     safeFilename,
			OriginalPath: destPath,
			ReadOnly:     true,
			CreatedAt:    time.Now(),
		}
		s.store.AddUpload(upload)
		results = append(results, upload)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uploads": results})
}

func (s *Server) handleUploadDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}
	
	snap := s.store.Snapshot()
	for _, up := range snap.Uploads {
		if up.ID == in.ID {
			os.Remove(up.OriginalPath)
			break
		}
	}
	s.store.DeleteUpload(in.ID)

	// Also remove from attachments if it's currently attached
	for _, att := range snap.ActiveAttachments {
		if att.Type == state.AttachmentTypeUpload && att.ID == in.ID {
			fullPath := filepath.Join(s.root, att.VirtualPath)
			os.Remove(fullPath)
			s.store.RemoveActiveAttachment(att.ID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUploadReadOnly(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		ReadOnly bool   `json:"read_only"`
	}
	if !decode(w, r, &in) {
		return
	}
	
	s.store.UpdateUpload(in.ID, func(up *state.UploadState) {
		up.ReadOnly = in.ReadOnly
	})
	
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUploadDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	
	snap := s.store.Snapshot()
	var masterPath string
	var filename string
	for _, up := range snap.Uploads {
		if up.ID == id {
			masterPath = up.OriginalPath
			filename = up.Filename
			break
		}
	}
	
	if masterPath == "" {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}
	
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	http.ServeFile(w, r, masterPath)
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs  []string `json:"ids"`
		Type string   `json:"type"`
	}
	if !decode(w, r, &in) {
		return
	}

	sessionID := s.store.Snapshot().SessionID
	attachDir := filepath.Join(s.root, ".loa", "attachments", sessionID)
	os.MkdirAll(attachDir, 0755)

	snap := s.store.Snapshot()
	var attached []state.AttachmentState
	
	for _, id := range in.IDs {
		var sourcePath string
		var filename string
		var attType state.AttachmentType
		var exists bool

		for _, a := range snap.ActiveAttachments {
			if a.ID == id {
				exists = true
				break
			}
		}
		if exists {
			continue
		}

		if in.Type == "upload" {
			for _, up := range snap.Uploads {
				if up.ID == id {
					sourcePath = up.OriginalPath
					filename = up.Filename
					attType = state.AttachmentTypeUpload
					break
				}
			}
		} else if in.Type == "artifact" {
			sourcePath = filepath.Join(s.root, id)
			filename = filepath.Base(id)
			attType = state.AttachmentTypeArtifact
		}

		if sourcePath == "" {
			continue
		}

		destPath := filepath.Join(attachDir, filename)
		sourceFile, err := os.Open(sourcePath)
		if err != nil {
			continue
		}
		
		destFile, err := os.Create(destPath)
		if err == nil {
			io.Copy(destFile, sourceFile)
			destFile.Close()
		}
		sourceFile.Close()

		att := state.AttachmentState{
			ID:          id,
			VirtualPath: filepath.Join(".loa", "attachments", sessionID, filename),
			Type:        attType,
			CreatedAt:   time.Now(),
		}
		s.store.AddActiveAttachment(att)
		attached = append(attached, att)
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "attached": attached})
}

func (s *Server) handleAttachDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &in) {
		return
	}

	snap := s.store.Snapshot()
	for _, att := range snap.ActiveAttachments {
		if att.ID == in.ID {
			fullPath := filepath.Join(s.root, att.VirtualPath)
			os.Remove(fullPath)
			break
		}
	}

	s.store.RemoveActiveAttachment(in.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
