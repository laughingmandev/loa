package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/embedder"
	"github.com/laughingmandev/loa/internal/index"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

var ErrBusy = errors.New("agent is currently busy")

type Status struct {
	Running           bool               `json:"running"`
	Reason            string             `json:"reason,omitempty"`
	ProjectRoot       string             `json:"project_root"`
	LastContext       string             `json:"last_context"`
	LastError         string             `json:"last_error,omitempty"`
	Stats             state.RuntimeStats `json:"stats"`
	IndexAvailable    bool               `json:"index_available"`
	IndexWarning      string             `json:"index_warning,omitempty"`
	NeedsProjectSetup bool               `json:"needs_project_setup,omitempty"`
}

type Engine struct {
	root     string
	store    *state.Store
	memStore      *memory.Store
	sessionStores map[string]*memory.Store
	cfg           *config.Manager
	llm           llm.Client
	localEmbedder embedder.Embedder
	tools         *tools.Manager
	index         *index.Index

	mu                            sync.Mutex
	localEmbeddings               atomic.Uint64
	loadedLocalEmbeddingModelPath string
	running                       bool
	systemTaskReason     string
	needsProjectSetup    bool
	pauseRequested       bool
	cancel               context.CancelFunc
	questionCh           chan state.Message
	interventionQueue    []state.Message
	approvals            map[uint64]approvalRequest
	runAllowed           map[string]struct{}
	inferenceByPrimitive map[string]uint64
	staleFiles           map[string]bool
	lastContext          string
	lastError            string
}

type approvalResolution struct {
	approved   bool
	superseded bool
}

type engineContextBuilder struct {
	e *Engine
}

func (b *engineContextBuilder) BuildPrompt(req primitives.PromptRequest) string {
	res, err := buildPrompt(b.e.root, b.e.cfg.Get(), b.e.store.Snapshot(), baseSystem, req)
	if err != nil {
		b.e.log(state.LogSystem, "BuildPrompt", "budget constraint violated", "", err.Error())
		// Fallback or truncate? We return whatever we have but log the error
		return res
	}
	return res
}

func (b *engineContextBuilder) RecordInference(primitive string, isRepair bool) {
	if isRepair {
		b.e.recordInference("JSONRepair", 1)
	} else {
		b.e.recordInference(primitive, 1)
	}
}

func (b *engineContextBuilder) Log(kind state.LogKind, primitive, summary, prompt, response string) {
	b.e.setLastContext(prompt)
	b.e.log(kind, primitive, summary, prompt, response)
}

// Approval request pending validation
type approvalRequest struct {
	ch     chan approvalResolution
	runKey string
}

func New(root string, st *state.Store, mem *memory.Store, cfg *config.Manager, lc llm.Client, tm *tools.Manager, idx *index.Index) *Engine {
	if tm != nil {
		tm.GetTask = func(id uint64) (*state.TaskState, error) {
			snap := st.Snapshot()
			if snap.ActiveTask != nil && snap.ActiveTask.ID == id {
				return snap.ActiveTask, nil
			}
			return nil, errors.New("task not found or not active")
		}
		tm.GetSessionID = func() string {
			snap := st.Snapshot()
			return snap.SessionID
		}
	}
	e := &Engine{
		root:                 root,
		store:                st,
		memStore:             mem,
		sessionStores:        make(map[string]*memory.Store),
		cfg:                  cfg,
		llm:                  lc,
		tools:                tm,
		index:                idx,
		approvals:            map[uint64]approvalRequest{},
		runAllowed:           map[string]struct{}{},
		inferenceByPrimitive: map[string]uint64{},
	}
	
	if cfg.Get().EmbeddingEngine == "local" && cfg.Get().LocalEmbeddingModelPath != "" {
		if emb, err := embedder.NewLocalEmbedder(cfg.Get().LocalEmbeddingModelPath); err == nil {
			e.localEmbedder = emb
			e.loadedLocalEmbeddingModelPath = cfg.Get().LocalEmbeddingModelPath
		} else {
			e.log(state.LogSystem, "Init", "failed to initialize local embedder", cfg.Get().LocalEmbeddingModelPath, err.Error())
		}
	}
	st.PlanMutationLogger = func(stack string, before, after []byte) {
		e.log(state.LogSystem, "PlanMutation", "Plan was mutated by backend process", "", "BEFORE:\n"+string(before)+"\n\nAFTER:\n"+string(after)+"\n\nSTACK TRACE:\n"+stack)
	}
	return e
}

func (e *Engine) MemStore() *memory.Store {
	return e.memStore
}

func (e *Engine) SessionMemStore() *memory.Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	sessionID := e.store.SessionStatus().ActiveID
	if sessionID == "" {
		return nil
	}
	if s, ok := e.sessionStores[sessionID]; ok {
		return s
	}
	path := filepath.Join(e.root, ".loa", "sessions", sessionID+"_memory.json")
	c := e.cfg.Get()
	s := memory.NewSessionStore(path, c.MemorySessionDecayHalfLifeHours, c.MemorySessionMaxWeight)
	_ = s.Load()
	e.sessionStores[sessionID] = s
	return s
}

func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	stats := e.llm.Stats()
	stats.Embeddings += e.localEmbeddings.Load()
	stats.InferenceByPrimitive = make(map[string]uint64, len(e.inferenceByPrimitive))
	for k, v := range e.inferenceByPrimitive {
		stats.InferenceByPrimitive[k] = v
	}
	s := Status{Running: e.running, Reason: e.systemTaskReason, ProjectRoot: e.root, LastContext: e.lastContext, LastError: e.lastError, Stats: stats, NeedsProjectSetup: e.needsProjectSetup}
	if e.index != nil {
		s.IndexAvailable = e.index.Available()
		s.IndexWarning = e.index.Warning()
	}
	return s
}

func (e *Engine) SetNeedsProjectSetup(b bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.needsProjectSetup = b
}

func (e *Engine) SubmitMessage(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("message is empty")
	}
	e.mu.Lock()
	if e.questionCh != nil {
		ch := e.questionCh
		e.mu.Unlock()
		m := state.Message{ID: e.store.NextID(), Source: state.MessageUser, Text: text, CreatedAt: time.Now()}
		e.store.AddMessage(m)
		select {
		case ch <- m:
			return nil
		default:
			return errors.New("clarification reply already queued")
		}
	}
	if e.running {
		snap := e.store.Snapshot()
		if snap.ActiveTask == nil || !taskIsInFlight(snap.ActiveTask.Status) {
			e.mu.Unlock()
			return ErrBusy
		}
		m := state.Message{ID: e.store.NextID(), Source: state.MessageUser, Text: text, CreatedAt: time.Now(), Intervention: true}
		e.interventionQueue = append(e.interventionQueue, m)
		for _, req := range e.approvals {
			select {
			case req.ch <- approvalResolution{superseded: true}:
			default:
			}
		}
		e.mu.Unlock()
		e.store.AddMessage(m)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.cancel = cancel
	e.lastError = ""
	e.mu.Unlock()
	m := state.Message{ID: e.store.NextID(), Source: state.MessageUser, Text: text, CreatedAt: time.Now()}
	e.store.AddMessage(m)
	go e.runMessage(ctx, m)
	return nil
}

func (e *Engine) Stop() {
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
}

func (e *Engine) Pause() {
	e.mu.Lock()
	e.pauseRequested = true
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
}

func (e *Engine) Resume() error {
	return e.StartActiveTask()
}

func (e *Engine) PurgeActiveTaskExtra(index int) {
	e.store.PurgeActiveTaskExtra(index)
}

func (e *Engine) LockSystemTask(reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = true
	e.systemTaskReason = reason
}

func (e *Engine) UnlockSystemTask() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.running = false
	e.systemTaskReason = ""
}

func (e *Engine) ReindexHardReset(ctx context.Context) error {
	e.LockSystemTask("indexing")
	defer e.UnlockSystemTask()

	e.log(state.LogSystem, "Reindex", "starting hard reset of file memories", "", "")

	// 1. Wipe all file-bound semantic memories
	if err := e.memStore.ClearFileMemories(); err != nil {
		e.log(state.LogSystem, "Reindex", "failed to clear old file memories", "", err.Error())
		return err
	}

	// 2. Refresh the AST index from disk
	if err := e.index.Rebuild(ctx); err != nil {
		e.log(state.LogSystem, "Reindex", "failed to rebuild ctags index", "", err.Error())
		return err
	}

	// 3. Kick off a completely fresh crawl.
	// Since all files were deleted from memStore, RunCrawler will index everything from scratch.
	if err := e.RunCrawler(ctx); err != nil {
		e.log(state.LogSystem, "Reindex", "crawler failed during hard reset", "", err.Error())
		return err
	}

	e.log(state.LogSystem, "Reindex", "hard reset completed successfully", "", "")
	return nil
}

func (e *Engine) Approve(toolCallID uint64, approved, allowRun bool) error {
	e.mu.Lock()
	req, ok := e.approvals[toolCallID]
	if !ok {
		e.mu.Unlock()
		return errors.New("approval request not found")
	}
	if allowRun {
		if !approved {
			e.mu.Unlock()
			return errors.New("allow-run requires approval")
		}
		if req.runKey == "" {
			e.mu.Unlock()
			return errors.New("allow-run is only available for shell/process commands")
		}
		e.runAllowed[req.runKey] = struct{}{}
	}
	delete(e.approvals, toolCallID)
	e.mu.Unlock()
	e.store.SetToolApproval(toolCallID, approved)
	select {
	case req.ch <- approvalResolution{approved: approved}:
		return nil
	default:
		return errors.New("approval already resolved")
	}
}

func (e *Engine) finishRun(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		e.addAgentMessage("Loa stopped with an error: "+err.Error(), false)
	}
	snap := e.store.Snapshot()
	if snap.ActiveTask != nil && taskIsInFlight(snap.ActiveTask.Status) {
		if errors.Is(err, context.Canceled) {
			e.mu.Lock()
			wasPaused := e.pauseRequested
			e.pauseRequested = false
			e.mu.Unlock()
			if wasPaused {
				e.store.PauseActiveTask()
			} else {
				e.store.CompleteActiveTask(state.TaskCancelled)
			}
		} else if err != nil {
			e.store.PauseActiveTaskWithError(err.Error())
		}
	}
	if err != nil {
		_ = e.store.Save("autosave")
	}
	e.mu.Lock()
	if err != nil {
		e.lastError = fmt.Sprintf("%+v", err)
	}
	e.running = false
	e.cancel = nil
	e.questionCh = nil
	e.interventionQueue = nil
	e.mu.Unlock()
}

func taskIsInFlight(status state.TaskStatus) bool {
	return status == state.TaskPlanning || status == state.TaskRunning || status == state.TaskWaiting
}

func (e *Engine) runMessage(ctx context.Context, msg state.Message) {
	var runErr error
	defer func() { e.finishRun(runErr) }()
	if e.cfg.Get().ModelConversation == "" {
		runErr = errors.New("no conversation model configured")
		return
	}

	snap := e.store.Snapshot()
	mode := snap.ComplexityMode
	mayModify := !snap.ReadOnly
	e.log(state.LogSystem, "RunMessage", fmt.Sprintf("Starting message run. Snapshot ComplexityMode: %q, ReadOnly: %v", mode, snap.ReadOnly), "", "")

	var intent state.IntentResult
	if mode == "" || mode == "Auto" {
		var err error
		intent, err = e.interpretIntent(ctx, msg.Text)
		if err != nil {
			runErr = err
			return
		}
		e.store.SetMessageIntents(msg.ID, intent.Intents)

		if intent.Ambiguous {
			ar, err := e.resolveAmbiguity(ctx, msg.Text)
			if err != nil {
				runErr = err
				return
			}
			if ar.Action == "ask_user" {
				answer, err := e.askUser(ctx, ar.Missing)
				if err != nil {
					runErr = err
					return
				}
				intent, err = e.interpretIntent(ctx, "Original message: "+msg.Text+"\nClarification answer: "+answer)
				if err != nil {
					runErr = err
					return
				}
			}
		}

		if hasIntent(intent.Intents, state.IntentTask) {
			mode = "Planned"
		} else {
			mode = "Fast"
		}
	} else {
		// Bypass interpretIntent for explicit mode
		intent = state.IntentResult{
			Intents:   []state.Intent{state.IntentTask},
			TaskTitle: "Process Request",
		}
		e.store.SetMessageIntents(msg.ID, intent.Intents)
	}

	_ = e.extractDurableMemory(ctx, []uint64{msg.ID})

	if snap.ActiveTask != nil && snap.ActiveTask.Status == state.TaskAwaitingApproval {
		runErr = e.runNegotiation(ctx, msg.Text, intent)
		return
	}

	if intent.TaskTitle == "" {
		intent.TaskTitle = "Process Request"
	}

	if snap.ActiveTask != nil && snap.ActiveTask.Status == state.TaskFailed && snap.ActiveTask.Failure != nil && snap.ActiveTask.Failure.Recoverable {
		isFastTask := len(snap.ActiveTask.Plan.Steps) > 0 && snap.ActiveTask.Plan.Steps[0].Title == "Execute Fast Track"
		modeMismatch := (mode == "Planned" && isFastTask) || (mode == "Fast" && !isFastTask)

		if modeMismatch {
			e.store.CompleteActiveTask(state.TaskFailed)
		} else {
			runErr = e.handleFailedTaskMessage(ctx, msg.Text, mayModify)
			return
		}
	}
	runErr = e.runTask(ctx, msg.Text, mayModify, intent.TaskTitle, mode)
}

func hasIntent(in []state.Intent, want state.Intent) bool {
	for _, v := range in {
		if v == want {
			return true
		}
	}
	return false
}

func (e *Engine) takeInterventions() []state.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.interventionQueue) == 0 {
		return nil
	}
	out := append([]state.Message(nil), e.interventionQueue...)
	e.interventionQueue = nil
	return out
}

func (e *Engine) hasPendingIntervention() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.interventionQueue) > 0
}

func (e *Engine) processInterventions(ctx context.Context, step *state.PlanStep) (string, string, error) {
	messages := e.takeInterventions()
	if len(messages) == 0 {
		return "none", "", nil
	}
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil || !taskIsInFlight(snap.ActiveTask.Status) {
		return "", "", errors.New("user guidance arrived without an active task")
	}
	combinedText := ""
	for _, msg := range messages {
		combinedText += msg.Text + "\n"
	}
	purpose := "USER GUIDANCE RECEIVED DURING ACTIVE TASK:\n" + mustJSON(messages)

	// Phase 4.4: Discussion Pre-fetch
	if cands, err := e.prefetchDiscussionMemory(ctx, combinedText, step); err == nil && len(cands) > 0 {
		purpose += "\n\nRELEVANT MEMORY (Pre-fetched based on guidance):\n" + cands
	} else if err != nil {
		e.log(state.LogSystem, "Intervention", "failed to prefetch memory", err.Error(), "")
	}

	purpose += fmt.Sprintf("\n\nTASK MODIFICATION AUTHORIZATION: %v", snap.ActiveTask.MayModify)
	if step != nil {
		purpose += "\n\nCURRENT STEP AT GUIDANCE BOUNDARY:\n" + mustJSON(step)
	}
	prim := primitives.NewInterventionPrimitive(e.cfg.Get(), e.llm, baseSystem)
	input := primitives.InterventionInput{Purpose: purpose}
	if step != nil {
		input.StepJSON = mustJSON(step)
	}
	decision, err := prim.Execute(ctx, &engineContextBuilder{e: e}, input)
	if err != nil {
		return "", "", err
	}

	guidance := make([]state.TaskGuidance, 0, len(messages))
	messageIDs := make([]uint64, 0, len(messages))
	latestParts := make([]string, 0, len(messages))
	for _, msg := range messages {
		guidance = append(guidance, state.TaskGuidance{MessageID: msg.ID, Text: msg.Text, CreatedAt: msg.CreatedAt})
		messageIDs = append(messageIDs, msg.ID)
		latestParts = append(latestParts, msg.Text)
	}
	e.store.UpdateActiveTask(func(t *state.TaskState) {
		t.Guidance = append(t.Guidance, guidance...)
		t.Context.Facts = appendUnique(t.Context.Facts, decision.Facts...)
		t.Context.Decisions = appendUnique(t.Context.Decisions, decision.Decisions...)
		t.Context.Constraints = appendUnique(t.Context.Constraints, decision.Constraints...)
		t.Context.OpenIssues = appendUnique(t.Context.OpenIssues, decision.OpenIssues...)
	})
	_ = e.extractDurableMemory(ctx, messageIDs)

	if decision.Action == "ask_user" {
		answer, err := e.askUser(ctx, decision.Missing)
		if err != nil {
			return "", "", err
		}
		e.store.UpdateActiveTask(func(t *state.TaskState) {
			t.Guidance = append(t.Guidance, state.TaskGuidance{Text: "Clarification: " + answer, CreatedAt: time.Now()})
		})
		latestParts = append(latestParts, "Clarification: "+answer)
		decision.Action = "replan_remaining"
		decision.Reason = "User guidance required clarification before remaining work could continue. " + decision.Reason
	}
	if decision.Action == "replan_remaining" {
		if snap.ActiveTask.ComplexityMode == "Fast" {
			decision.Action = "continue"
			decision.Reason = "replan skipped due to Fast Mode. " + decision.Reason
		} else {
			if err := e.updateAcceptanceCriteria(ctx, snap.ActiveTask.Goal, strings.Join(latestParts, "\n"), nil); err != nil {
				return "", "", err
			}
			return "replan", "User guidance changed the active task: " + decision.Reason, nil
		}
	}
	return "continue", decision.Reason, nil
}

func (e *Engine) interpretIntent(ctx context.Context, text string) (state.IntentResult, error) {
	cb := &engineContextBuilder{e: e}
	prim := primitives.NewIntentPrimitive(e.cfg.Get(), e.llm, baseSystem)
	return prim.Execute(ctx, cb, primitives.IntentInput{UserText: text})
}

func (e *Engine) resolveAmbiguity(ctx context.Context, text string) (primitives.AmbiguityResult, error) {
	cb := &engineContextBuilder{e: e}
	prim := primitives.NewAmbiguityPrimitive(e.cfg.Get(), e.llm, baseSystem)
	return prim.Execute(ctx, cb, primitives.AmbiguityInput{UserText: text})
}




func (e *Engine) runNegotiation(ctx context.Context, userText string, intent state.IntentResult) error {
	extras := []string{}
	retrievalRound := 0
	deniedSignature := ""

	snap := e.store.Snapshot()
	planContext := "CURRENT PENDING PLAN:\n" + mustJSON(snap.ActiveTask.Plan) + "\n\n"

	for loop := 0; loop < e.cfg.Get().MaxExecutionLoops; loop++ {
		purpose := planContext + fmt.Sprintf("USER REQUEST:\n%s\n\nINTENT:\n%s\n\nAVAILABLE TOOLS:\n%s", userText, mustJSON(intent), tools.ToolSchema(snap.ReadOnly))
		negPrim := primitives.NewNegotiationPrimitive(e.cfg.Get(), e.llm, baseSystem)
		d, err := negPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.NegotiationInput{
			Purpose: purpose,
			Extras:  extras,
		})
		if err != nil {
			return err
		}
		switch d.Action {
		case "approve":
			e.addAgentMessage(d.Reason, false)
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				t.Status = state.TaskRunning
			})
			return e.runActiveTask(ctx)
		case "replan":
			e.addAgentMessage("Replanning based on your feedback: "+d.Reason, false)
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				t.Status = state.TaskPlanning
				t.Goal += "\n\nUSER FEEDBACK FOR REPLAN:\n" + d.ReplanInstructions
			})
			return e.runActiveTask(ctx)
		case "retrieve_memory":

			deniedSignature = ""
			cands, err := e.retrieveMemoryRound(ctx, d.Query, d.AllowedKinds, nil, retrievalRound)
			retrievalRound++
			if err != nil {
				extras = append(extras, "Memory retrieval error: "+err.Error())
			} else {
				extras = append(extras, "MEMORY RESULTS:\n"+cands)
			}
		case "task_summaries":

			deniedSignature = ""
			extras = append(extras, e.taskSummaryText())
		case "tool":

			if d.Tool == nil {
				return errors.New("negotiation requested nil tool")
			}
			sig := toolSignature(*d.Tool)
			if sig == deniedSignature {
				extras = append(extras, "Repeated identical tool request blocked because the user just denied it. Choose a different action, gather more information, or ask the user.")
				continue
			}
			if tools.IsMutating(d.Tool.Kind) && d.Tool.Kind != state.ToolExecuteProcess && d.Tool.Kind != state.ToolExecuteShell {
				return fmt.Errorf("negotiation mode rejected mutating tool %s", d.Tool.Kind)
			}
			res, err := e.executeTool(ctx, *d.Tool, false)
			if err != nil {
				return err
			}
			if res.Error == "permission_denied" {
				deniedSignature = sig
			} else {
				deniedSignature = ""
			}
			extras = append(extras, formatToolResult(*d.Tool, res))
		case "ask_user":
			deniedSignature = ""
			answer, err := e.askUser(ctx, d.Question)
			if err != nil {
				return err
			}
			extras = append(extras, "USER CLARIFICATION: "+answer)
		case "final":
			answer, err := e.generateFinalResponse(ctx, userText, d.Answer, nil, extras)
			if err != nil {
				return err
			}
			e.addAgentMessage(answer, false)
			return nil
		}
	}
	return errors.New("negotiation exceeded maximum execution loops")
}

func (e *Engine) runTask(ctx context.Context, goal string, mayModify bool, title string, complexityMode string) error {
	if snap := e.store.Snapshot(); snap.ActiveTask != nil {
		return errors.New("cannot start a new task while another task is active")
	}

	taskID := e.store.NextID()
	task := &state.TaskState{ID: taskID, Title: title, Goal: goal, MayModify: mayModify, ComplexityMode: complexityMode, Status: state.TaskPlanning, StartedAt: time.Now(), Plan: state.Plan{Version: 1}}
	e.store.SetActiveTask(task)

	planningExtras := []string{}
	if e.cfg.Get().EmbeddingEngine == "local" || e.cfg.Get().EmbeddingModel != "" {
		if cands, err := e.retrieveMemory(ctx, goal, nil); err == nil && len(cands) > 0 {
			planningExtras = append(planningExtras, "RELEVANT PRIOR MEMORY FOR PLANNING:\n"+cands)
		}
	}
	
	if complexityMode == "Fast" {
		stepID := e.store.NextID()
		mode := state.StepModify
		if !mayModify {
			mode = state.StepInvestigate
		}
		step := state.PlanStep{
			ID:     stepID,
			Title:  "Execute Fast Track",
			Goal:   goal,
			Reason: "Fast mode enabled. Proceed dynamically.",
			Mode:   mode,
			Status: state.StepPending,
		}
		e.store.UpdateActiveTask(func(t *state.TaskState) {
			t.Plan.Steps = append(t.Plan.Steps, step)
		})
	} else {
		if err := e.updateAcceptanceCriteria(ctx, goal, "", planningExtras); err != nil {
			return err
		}
		if err := e.createInitialPlan(ctx, goal, planningExtras); err != nil {
			return err
		}
	}

	if mayModify && complexityMode != "Fast" {
		e.store.UpdateActiveTask(func(t *state.TaskState) {
			t.Status = state.TaskAwaitingApproval
		})
		e.log(state.LogSystem, "Task", "Awaiting approval for execution plan", "", "")
		return nil
	}

	e.store.UpdateActiveTask(func(t *state.TaskState) {
		t.Status = state.TaskRunning
	})
	
	return e.runActiveTask(ctx)
}

func (e *Engine) StartActiveTask() error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return errors.New("agent is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.running = true
	e.cancel = cancel
	e.lastError = ""
	e.mu.Unlock()

	e.store.UpdateActiveTask(func(t *state.TaskState) {
		if t.Status == state.TaskPaused || t.Status == state.TaskAwaitingApproval {
			t.Status = state.TaskRunning
		}
	})

	go func() {
		var err error
		defer func() { e.finishRun(err) }()
		if err = e.runActiveTask(ctx); err != nil {
			e.log(state.LogSystem, "Task Error", err.Error(), "", "")
		}
	}()
	return nil
}

func (e *Engine) ApprovePlan(ctx context.Context, approved bool) error {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil || snap.ActiveTask.Status != state.TaskAwaitingApproval {
		return errors.New("no task is currently awaiting approval")
	}

	if !approved {
		e.store.FailActiveTask("User rejected the execution plan.")
		return nil
	}

	return e.StartActiveTask()
}

func (e *Engine) handleFailedTaskMessage(ctx context.Context, latest string, mayModify bool) error {
	for attempts := 0; attempts < 3; attempts++ {
		snap := e.store.Snapshot()
		if snap.ActiveTask == nil || snap.ActiveTask.Status != state.TaskFailed {
			return errors.New("recoverable failed task disappeared")
		}
		purpose := fmt.Sprintf("FAILED TASK:\n%s\n\nLATEST USER MESSAGE:\n%s", mustJSON(snap.ActiveTask), latest)
		recPrim := primitives.NewAssessTaskRecoveryPrimitive(e.cfg.Get(), e.llm, baseSystem)
		decision, err := recPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.AssessTaskRecoveryInput{
			Purpose: purpose,
		})
		if err != nil {
			return err
		}

		switch decision.Action {
		case "ask_user":
			answer, err := e.askUser(ctx, decision.Missing)
			if err != nil {
				return err
			}
			latest += "\nClarification: " + answer
			continue
		case "start_new":
			e.store.CompleteActiveTask(state.TaskFailed)
			return e.runTask(ctx, latest, mayModify, "", snap.ComplexityMode)
		case "continue", "retry", "replan":
			if err := e.updateAcceptanceCriteria(ctx, snap.ActiveTask.Goal, latest, nil); err != nil {
				return err
			}
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				t.Status = state.TaskRunning
				if t.Failure != nil {
					t.Failure.Recoverable = false
				}
				if t.Failure != nil && t.Failure.FailedStepID != nil && (decision.Action == "continue" || decision.Action == "retry") {
					for i := range t.Plan.Steps {
						if t.Plan.Steps[i].ID == *t.Failure.FailedStepID && t.Plan.Steps[i].Status == state.StepFailed {
							t.Plan.Steps[i].Status = state.StepPending
						}
					}
				}
			})
			if decision.Action == "replan" {
				reason := "Recovering from a preserved hard failure. " + decision.Reason + " Latest user message: " + latest
				if err := e.replan(ctx, reason); err != nil {
					return err
				}
			}
			return e.runActiveTask(ctx)
		}
	}
	return errors.New("failed task recovery remained ambiguous")
}

func (e *Engine) runActiveTask(ctx context.Context) error {
	replans := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.hasStaleFiles() {
			if err := e.repairStaleFiles(ctx); err != nil {
				return err
			}
		}
		snap := e.store.Snapshot()
		if snap.ActiveTask == nil {
			return errors.New("active task disappeared")
		}
		idx, step := nextPending(snap.ActiveTask.Plan.Steps)
		interventionOutcome, interventionReason, err := e.processInterventions(ctx, step)
		if err != nil {
			return err
		}
		if interventionOutcome == "replan" {
			replans++
			if replans > e.cfg.Get().MaxPlanDepth {
				return errors.New("maximum replans exceeded")
			}
			if err := e.replan(ctx, interventionReason); err != nil {
				return err
			}
			continue
		}
		if step == nil {
			check, err := e.runIterativeEvaluation(ctx, !snap.ActiveTask.MayModify)
			if err != nil {
				return err
			}
			interventionOutcome, interventionReason, err = e.processInterventions(ctx, nil)
			if err != nil {
				return err
			}
			if interventionOutcome == "replan" {
				replans++
				if replans > e.cfg.Get().MaxPlanDepth {
					return errors.New("maximum replans exceeded")
				}
				if err := e.replan(ctx, interventionReason); err != nil {
					return err
				}
				continue
			}
			if interventionOutcome != "none" {
				continue // final check was produced before the newly consumed guidance
			}
			if !check.Complete {
				replans++
				if replans > e.cfg.Get().MaxPlanDepth {
					return fmt.Errorf("final completion check repeatedly failed: %s", check.Reason)
				}
				reason := check.Reason
				if len(check.Missing) > 0 {
					reason += "; " + strings.Join(check.Missing, "; ")
				}
				if err := e.replan(ctx, "Final completion check failed: "+reason); err != nil {
					return err
				}
				continue
			}
			err = e.finishTask(ctx, check)
			if err != nil && err.Error() == "continue_synthesis" {
				continue
			}
			return err
		}
		if err := e.prepareStep(ctx, idx, *step); err != nil {
			return err
		}
		runningID, err := e.runningStepID()
		if err != nil {
			return err
		}
		runningStep, err := e.getStep(runningID)
		if err != nil {
			return err
		}
		interventionOutcome, interventionReason, err = e.processInterventions(ctx, &runningStep)
		if err != nil {
			return err
		}
		if interventionOutcome == "replan" {
			replans++
			if replans > e.cfg.Get().MaxPlanDepth {
				return errors.New("maximum replans exceeded")
			}
			if err := e.replan(ctx, interventionReason); err != nil {
				return err
			}
			continue
		}
		outcome, reason, err := e.executeStep(ctx, runningID)
		if err != nil {
			if strings.Contains(err.Error(), "structured response invalid after") {
				replans++
				if replans > e.cfg.Get().MaxPlanDepth {
					return errors.New("maximum replans exceeded due to repeated output formatting failures")
				}
				if err := e.replan(ctx, "Model repeatedly failed to format or complete its output: "+err.Error()); err != nil {
					return err
				}
				continue
			}
			return err
		}
		switch outcome {
		case "fast_complete":
			return nil
		case "done":
			continue
		case "replan":
			replans++
			if replans > e.cfg.Get().MaxPlanDepth {
				return errors.New("maximum replans exceeded")
			}
			if err := e.replan(ctx, reason); err != nil {
				return err
			}
		case "cancel":
			return context.Canceled
		default:
			return fmt.Errorf("unknown step outcome %q", outcome)
		}
	}
}

func (e *Engine) updateAcceptanceCriteria(ctx context.Context, originalGoal, latest string, extras []string) error {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return errors.New("no active task")
	}
	purpose := "ORIGINAL TASK:\n" + originalGoal
	if len(snap.ActiveTask.AcceptanceCriteria) > 0 {
	}
	if strings.TrimSpace(latest) != "" {
		purpose += "\n\nLATEST USER REQUEST ABOUT THIS TASK:\n" + latest
	}
	cb := &engineContextBuilder{e: e}
	prim := primitives.NewCriteriaPrimitive(e.cfg.Get(), e.llm, baseSystem)
	draft, err := prim.Execute(ctx, cb, primitives.CriteriaInput{
		Purpose: purpose,
		Extras:  extras,
	})
	if err != nil {
		return err
	}
	criteria := make([]state.AcceptanceCriterion, 0, len(draft.Criteria))
	for i, text := range draft.Criteria {
		criteria = append(criteria, state.AcceptanceCriterion{ID: uint64(i + 1), Requirement: strings.TrimSpace(text)})
	}

	depPrim := primitives.NewCriteriaDependencyPrimitive(e.cfg.Get(), e.llm, baseSystem)
	depResult, err := depPrim.Execute(ctx, cb, primitives.CriteriaDependencyInput{
		Purpose:  purpose,
		Criteria: criteria,
	})
	if err == nil {
		depMap := make(map[uint64][]uint64)
		for _, m := range depResult.Mappings {
			depMap[m.CriterionID] = m.DependsOn
		}
		for i := range criteria {
			if deps, ok := depMap[criteria[i].ID]; ok {
				criteria[i].DependsOn = deps
			}
		}
	} else {
		e.log(state.LogSystem, "DefineCriteriaDependencies", "dependency enrichment failed, falling back to flat list", "", err.Error())
	}

	e.store.UpdateActiveTask(func(t *state.TaskState) {
		t.AcceptanceCriteria = criteria
		t.AcceptanceChecks = nil
	})
	return nil
}

func (e *Engine) runningStepID() (uint64, error) {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return 0, errors.New("no active task")
	}
	for _, s := range snap.ActiveTask.Plan.Steps {
		if s.Status == state.StepRunning {
			return s.ID, nil
		}
	}
	return 0, errors.New("no running plan step")
}

func nextPending(steps []state.PlanStep) (int, *state.PlanStep) {
	for i := range steps {
		if steps[i].Status == state.StepPending || steps[i].Status == state.StepRunning {
			s := steps[i]
			return i, &s
		}
	}
	return -1, nil
}

func (e *Engine) tagPlanDraftTargets(ctx context.Context, draft *primitives.PlanResult) error {
	purpose := "PLAN DRAFT:\n" + mustJSON(draft)
	tagPrim := primitives.NewTagTargetsPrimitive(e.cfg.Get(), e.llm, baseSystem)
	targets, err := tagPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.TagTargetsInput{Purpose: purpose})
	if err != nil {
		e.log(state.LogPlan, "TagTargets", "failed to tag plan targets", err.Error(), "")
		return err
	}
	for _, tag := range targets.Tags {
		if tag.StepID >= 1 && tag.StepID <= len(draft.Steps) {
			draft.Steps[tag.StepID-1].TargetEntities = tag.Entities
		}
	}
	return nil
}

func (e *Engine) createInitialPlan(ctx context.Context, goal string, planningExtras []string) error {
	snap := e.store.Snapshot()
	if snap.ActiveTask != nil && snap.ActiveTask.Target == "" {
		targetPrim := primitives.NewSynthesizeTargetPrimitive(e.cfg.Get(), e.llm, baseSystem)
		t, err := targetPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.SynthesizeTargetInput{
			Goal:    goal,
			Purpose: "Synthesizing target for initial plan.",
		})
		if err != nil {
			e.log(state.LogPlan, "SynthesizeTarget", "failed to synthesize target", err.Error(), "")
		} else if t != "" {
			e.store.UpdateActiveTask(func(ts *state.TaskState) {
				ts.Target = t
			})
			e.log(state.LogPlan, "SynthesizeTarget", "target synthesized", t, "")
		}
	}

	critique := ""
	for attempt := 0; attempt < e.cfg.Get().MaxPlanDepth; attempt++ {

		snap := e.store.Snapshot()
		purpose := "TASK GOAL:\n" + goal
		if snap.ActiveTask != nil {
			purpose += fmt.Sprintf("\n\nTASK MODIFICATION AUTHORIZATION: %v", snap.ActiveTask.MayModify)
		}
		if critique != "" {
			purpose += "\n\nPREVIOUS PLAN CRITIQUE:\n" + critique
		}
		if snap.ActiveTask == nil {
			return errors.New("active task disappeared during planning")
		}
		
		cb := &engineContextBuilder{e: e}
		prim := primitives.NewPlanPrimitive(e.cfg.Get(), e.llm, baseSystem)
		draft, err := prim.Execute(ctx, cb, primitives.PlanInput{
			Operation: "PlanTask",
			Purpose:   purpose,
			Extras:    planningExtras,
			MayModify: snap.ActiveTask.MayModify,
		})
		if err != nil {
			return err
		}

		if draft.Action == "ask_user" {
			ans, err := e.askUser(ctx, draft.AskUserQuestion)
			if err != nil {
				return err
			}
			planningExtras = append(planningExtras, "USER CLARIFICATION: "+ans)
			critique = ""
			attempt = -1 // so it resets to 0 on continue
			continue
		}

		// Phase 4.1: Pass 2 - Tag TargetEntities
		_ = e.tagPlanDraftTargets(ctx, &draft)

		plan := e.planFromDraft(draft, 1, 0, nil)

		evalPurpose := "TASK GOAL:\n" + goal
		if snap.ActiveTask != nil {
		}
		evalPrim := primitives.NewEvaluatePlanPrimitive(e.cfg.Get(), e.llm, baseSystem)
		ev, err := evalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluatePlanInput{
			Purpose:  evalPurpose,
			Extras:   planningExtras,
			Criteria: snap.ActiveTask.AcceptanceCriteria,
			Plan:     plan,
		})
		if err != nil {
			return err
		}
		if ev.Valid {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				t.Plan = plan
				t.Status = state.TaskRunning
			})
			e.addMemory(ctx, state.MemoryPlan, formatPlanForMemory(plan), nil, nil, state.MessageAgent)
			return nil
		}
		critique = ev.Reason + "\n" + strings.Join(ev.Issues, "\n")
	}
	return errors.New("could not produce an acceptable plan")
}

func (e *Engine) planFromDraft(d primitives.PlanResult, version uint64, depth int, inherited []uint64) state.Plan {
	ids := make([]uint64, len(d.Steps))
	for i := range ids {
		ids[i] = e.store.NextID()
	}
	steps := make([]state.PlanStep, len(d.Steps))
	for i, s := range d.Steps {
		deps := []uint64{}
		for _, pos := range s.DependsOn {
			if pos >= 1 && pos <= len(ids) {
				deps = append(deps, ids[pos-1])
			}
		}
		if len(deps) == 0 {
			deps = append(deps, inherited...)
		}
		steps[i] = state.PlanStep{ID: ids[i], Title: s.Title, Goal: s.Goal, Reason: s.Reason, ExpectedResult: s.ExpectedResult, Mode: s.Mode, DependsOn: deps, ExpectedTools: s.ExpectedTools, Status: state.StepPending, DecompositionDepth: depth, TargetEntities: s.TargetEntities}
	}
	return state.Plan{Version: version, Steps: steps}
}

func (e *Engine) prepareStep(ctx context.Context, idx int, step state.PlanStep) error {
	for {
		snap := e.store.Snapshot()
		if snap.ActiveTask == nil {
			return errors.New("active task disappeared")
		}
		if !snap.ActiveTask.MayModify && step.Mode == state.StepModify {
			return fmt.Errorf("read-only task cannot execute modify step %q", step.Title)
		}
		if snap.ActiveTask.ComplexityMode == "Fast" {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				for i := range t.Plan.Steps {
					if t.Plan.Steps[i].ID == step.ID {
						t.Plan.Steps[i].Status = state.StepRunning
					}
				}
			})
			return nil
		}
		stepEvalPrim := primitives.NewEvaluateStepPrimitive(e.cfg.Get(), e.llm, baseSystem)
		ev, err := stepEvalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluateStepInput{Step: step})
		if err != nil {
			return err
		}
		if ev.Executable {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				for i := range t.Plan.Steps {
					if t.Plan.Steps[i].ID == step.ID {
						t.Plan.Steps[i].Status = state.StepRunning
					}
				}
			})
			return nil
		}
		if step.DecompositionDepth >= e.cfg.Get().MaxPlanDepth {
			return fmt.Errorf("step %q remains too broad at maximum decomposition depth: %s", step.Title, ev.Reason)
		}

		decPrim := primitives.NewDecomposeStepPrimitive(e.cfg.Get(), e.llm, baseSystem)
		dec, err := decPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.DecomposeStepInput{
			StepJSON:  mustJSON(step),
			MayModify: snap.ActiveTask.MayModify,
			Eval:      ev.Reason,
		})
		if err != nil {
			return err
		}
		if len(dec.Steps) < 2 {
			return errors.New("decomposition must create at least two smaller steps")
		}

		draftDec := primitives.PlanResult{Steps: dec.Steps}
		_ = e.tagPlanDraftTargets(ctx, &draftDec)

		newPlan := e.planFromDraft(draftDec, 0, step.DecompositionDepth+1, step.DependsOn)
		e.store.UpdateActiveTask(func(t *state.TaskState) {
			pos := -1
			for i := range t.Plan.Steps {
				if t.Plan.Steps[i].ID == step.ID {
					pos = i
					break
				}
			}
			if pos < 0 {
				return
			}
			replacement := newPlan.Steps
			lastID := replacement[len(replacement)-1].ID
			t.Plan.Steps = append(append(append([]state.PlanStep{}, t.Plan.Steps[:pos]...), replacement...), t.Plan.Steps[pos+1:]...)
			
			for i := range t.Plan.Steps {
				for j, dep := range t.Plan.Steps[i].DependsOn {
					if dep == step.ID {
						t.Plan.Steps[i].DependsOn[j] = lastID
					}
				}
			}
			
			t.Plan.Version++
		})
		e.log(state.LogPlan, "DecomposeStep", "decomposed step: "+step.Title, mustJSON(step), mustJSON(dec))
		snap = e.store.Snapshot()
		_, next := nextPending(snap.ActiveTask.Plan.Steps)
		if next == nil {
			return errors.New("decomposition left no pending step")
		}
		step = *next
		idx = 0
		_ = idx
	}
}

func (e *Engine) replan(ctx context.Context, reason string) error {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return errors.New("no active task")
	}
	completed := []state.PlanStep{}
	for _, s := range snap.ActiveTask.Plan.Steps {
		if s.Status == state.StepCompleted {
			completed = append(completed, s)
		}
	}
	critique := reason
	for attempt := 0; attempt < e.cfg.Get().MaxPlanDepth; attempt++ {
		purpose := fmt.Sprintf("TASK GOAL:\n%s\n\nTASK MODIFICATION AUTHORIZATION: %v\n\nCOMPLETED STEPS (immutable history):\n%s\n\nWHY REPLAN IS NEEDED / CRITIQUE:\n%s\n\nCreate only the remaining steps. Preserve completed work unless current evidence shows it is invalid.", snap.ActiveTask.Goal, snap.ActiveTask.MayModify, mustJSON(completed), critique)
		cb := &engineContextBuilder{e: e}
		prim := primitives.NewPlanPrimitive(e.cfg.Get(), e.llm, baseSystem)
		draft, err := prim.Execute(ctx, cb, primitives.PlanInput{
			Operation: "Replan",
			Purpose:   purpose,
			MayModify: snap.ActiveTask.MayModify,
		})
		if err != nil {
			return err
		}

		if draft.Action == "ask_user" {
			ans, err := e.askUser(ctx, draft.AskUserQuestion)
			if err != nil {
				return err
			}
			critique = critique + "\nUSER CLARIFICATION: " + ans
			attempt = -1
			continue
		}

		_ = e.tagPlanDraftTargets(ctx, &draft)

		var inherited []uint64
		if len(completed) > 0 {
			inherited = append(inherited, completed[len(completed)-1].ID)
		}
		newPart := e.planFromDraft(draft, snap.ActiveTask.Plan.Version+1, 0, inherited)
		candidate := state.Plan{Version: newPart.Version, Steps: append(append([]state.PlanStep{}, completed...), newPart.Steps...)}

		evalInput := fmt.Sprintf("TASK GOAL:\n%s\n\nREPLANNED FULL PLAN:\n%s\n\nREPLAN TRIGGER:\n%s", snap.ActiveTask.Goal, mustJSON(candidate), reason)
		evalPrim := primitives.NewEvaluatePlanPrimitive(e.cfg.Get(), e.llm, baseSystem)
		ev, err := evalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluatePlanInput{
			Purpose:  evalInput,
			Criteria: snap.ActiveTask.AcceptanceCriteria,
			Plan:     candidate,
		})
		if err != nil {
			return err
		}
		if ev.Valid {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				t.Status = state.TaskPlanning
				t.Plan = candidate
				t.Status = state.TaskRunning
			})
			e.log(state.LogPlan, "Replan", "replaced pending plan steps", reason, mustJSON(candidate))
			return nil
		}
		critique = ev.Reason + "\n" + strings.Join(ev.Issues, "\n")
	}
	return errors.New("could not produce an acceptable replanned plan")
}

func (e *Engine) executeStep(ctx context.Context, stepID uint64) (string, string, error) {
	step, err := e.getStep(stepID)
	if err != nil {
		return "", "", err
	}
	alreadyInjected := false
	if sStore := e.SessionMemStore(); sStore != nil {
		for _, mem := range sStore.SessionMemories() {
			if mem.Kind == state.MemoryStep && mem.SourceStepID != nil && *mem.SourceStepID == step.ID {
				alreadyInjected = true
				break
			}
		}
	}
	
	if !alreadyInjected {
		e.addMemory(ctx, state.MemoryStep, step.Title+": "+step.Goal, nil, &step.ID, state.MessageAgent)
	}

	snap := e.store.Snapshot()
	var extras []string
	if snap.ActiveTask != nil && snap.ActiveTask.Extras != nil {
		extras = snap.ActiveTask.Extras
	} else {
		extras = []string{}
	}
	defer func() { e.store.SetActiveTaskExtras(extras) }()
	
	retrievalRound := 0
	deniedSignature := ""
	executedReads := map[string]bool{}
	readCount := 0
	filesRead := map[string]bool{}
	filesChanged := map[string]bool{}
	toolIDs := []uint64{}
	artifacts := []string{}
	blindTrail := []string{}
	consecutiveFailures := 0
	blindActions := 0
	pinnedEvaluation := ""
	for loop := 0; loop < e.cfg.Get().MaxExecutionLoops; loop++ {
		step, err := e.getStep(stepID)
		if err != nil {
			return "", "", err
		}
		if e.hasStaleFiles() {
			if err := e.repairStaleFiles(ctx); err != nil {
				e.log(state.LogSystem, "Engine", "failed to repair stale files during step execution", "", err.Error())
			}
		}
		interventionOutcome, interventionReason, err := e.processInterventions(ctx, &step)
		if err != nil {
			return "", "", err
		}
		if interventionOutcome == "replan" {
			return "replan", interventionReason, nil
		}
		execPrim := primitives.NewExecuteStepPrimitive(e.cfg.Get(), e.llm, baseSystem)
		
		stepExtras := make([]string, len(extras))
		copy(stepExtras, extras)
		if len(artifacts) > 0 {
			stepExtras = append(stepExtras, "ACTIVE ARTIFACTS CREATED IN THIS LOOP:\n"+strings.Join(artifacts, ", ")+"\nUse artifact_read to read them if you need to recall notes.")
		}

		d, err := execPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExecuteStepInput{
			Step:             step,
			PinnedEvaluation: pinnedEvaluation,
			Extras:           stepExtras,
			BlindTrail:       blindTrail,
			ReadOnly:         snap.ReadOnly,
			IsFastLane:       snap.ActiveTask.ComplexityMode == "Fast",
		}, e.cfg.Get().EvaluationMode)
		if err != nil {
			if strings.Contains(err.Error(), "structured response invalid after") {
				return "replan", "Model repeatedly failed to format or complete its output: " + err.Error(), nil
			}
			return "", "", err
		}
		interventionOutcome, interventionReason, err = e.processInterventions(ctx, &step)
		if err != nil {
			return "", "", err
		}
		if interventionOutcome == "replan" {
			return "replan", interventionReason, nil
		}
		if interventionOutcome != "none" || e.hasPendingIntervention() {
			continue // discard a decision produced before newer user guidance
		}

		switch d.Action {
		case "retrieve_memory":
			deniedSignature = ""
			cands, err := e.retrieveMemoryRound(ctx, d.Query, d.AllowedKinds, &step, retrievalRound)
			retrievalRound++
			if err != nil {
				extras = append(extras, "Memory retrieval error: "+err.Error())
			} else {
				extras = append(extras, "MEMORY RESULTS:\n"+cands)
			}
		case "task_summaries":
			deniedSignature = ""
			extras = append(extras, e.taskSummaryText())
		case "ask_user":
			deniedSignature = ""
			ans, err := e.askUser(ctx, d.AskUserQuestion)
			if err != nil {
				return "", "", err
			}
			extras = append(extras, "USER CLARIFICATION: "+ans)
		case "tool":
			if d.Tool == nil {
				return "", "", errors.New("executor requested nil tool")
			}
			sig := toolSignature(*d.Tool)
			if sig == deniedSignature {
				extras = append(extras, "Repeated identical tool request blocked because the user just denied it. Choose a different action, gather more information, or ask the user.")
				continue
			}
			if !tools.IsMutating(d.Tool.Kind) && executedReads[sig] {
				extras = append(extras, "You have already executed this exact tool request in this step. The result is already in your context or has been permanently recorded in the task context. Do not repeat it.")
				continue
			}
			snap := e.store.Snapshot()
			if snap.ActiveTask == nil {
				return "", "", errors.New("active task disappeared")
			}
			mayModify := snap.ActiveTask.MayModify
			if (!mayModify || step.Mode != state.StepModify) && tools.IsMutating(d.Tool.Kind) && d.Tool.Kind != state.ToolExecuteProcess && d.Tool.Kind != state.ToolExecuteShell {
				extras = append(extras, "Tool rejected: active task/current step is non-modifying and requested "+string(d.Tool.Kind))
				continue
			}
			
			if tools.IsMutating(d.Tool.Kind) && d.Tool.Kind != state.ToolExecuteProcess {

				hPurpose := fmt.Sprintf("INTENDED MUTATION:\n%s\n\nBefore executing this mutation, formulate a scientific hypothesis.", mustJSON(requestToMap(*d.Tool)))
				hypoPrim := primitives.NewFormulateHypothesisPrimitive(e.cfg.Get(), e.llm, baseSystem)
				h, err := hypoPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.FormulateHypothesisInput{
					Purpose: hPurpose,
					Step:    &step,
					Extras:  extras,
				})
				if err == nil {
					extras = append(extras, "SCIENTIFIC HYPOTHESIS:\n"+mustJSON(h))
				}
			}
			res, err := e.executeTool(ctx, *d.Tool, mayModify)
			if err != nil {
				return "", "", err
			}
			if res.Error == "intervention_superseded" {
				extras = append(extras, "Pending tool action was superseded by newer user guidance before execution.")
				continue
			}
			if res.Error == "permission_denied" {
				deniedSignature = sig
			} else {
				deniedSignature = ""
			}
			toolIDs = append(toolIDs, res.ToolCallID)
			trackToolFiles(*d.Tool, res, filesRead, filesChanged)
			if res.Success && (d.Tool.Kind == state.ToolWriteFile || d.Tool.Kind == state.ToolPatchFile || d.Tool.Kind == state.ToolDeleteFile) {
				var x struct{ Path string `json:"path"` }
				if err := json.Unmarshal(d.Tool.Input, &x); err == nil && x.Path != "" {
					e.markStale(filepath.Join(e.root, x.Path))
				}
			}
			if res.Success && (d.Tool.Kind == state.ToolArtifactWrite || d.Tool.Kind == state.ToolArtifactAppend || d.Tool.Kind == state.ToolArtifactPatch) {
				var x struct{ Name string `json:"name"` }
				if err := json.Unmarshal(d.Tool.Input, &x); err == nil && x.Name != "" {
					artifacts = appendUnique(artifacts, x.Name)
				}
			}
			extras = append(extras, formatToolResult(*d.Tool, res))
			blindTrail = append(blindTrail, fmt.Sprintf("- Action: %s, Reason: %s", string(d.Tool.Kind), d.Reason))
			if !tools.IsMutating(d.Tool.Kind) && res.Success {
				executedReads[sig] = true
				readCount++
				if readCount >= 8 {
					readCount = 0
					if err := e.checkpointStep(ctx, step, extras); err != nil {
						e.log(state.LogSystem, "CheckpointStep", "failed to checkpoint step", "", err.Error())
					}
				}
			}
			interventionOutcome, interventionReason, err = e.processInterventions(ctx, &step)
			if err != nil {
				return "", "", err
			}
			if interventionOutcome == "replan" {
				return "replan", interventionReason, nil
			}
			if interventionOutcome != "none" {
				continue // reconsider the step with the completed tool result plus new guidance
			}
			if tools.IsMutating(d.Tool.Kind) && d.Tool.Kind != state.ToolExecuteProcess {
				shouldEvaluate := true
				if e.cfg.Get().EvaluationMode == "Dynamic" {
					blindActions++
					if d.EvaluateAfter != nil && !*d.EvaluateAfter {
						shouldEvaluate = false
					}
					if blindActions >= e.cfg.Get().BlindActionThreshold && !shouldEvaluate {
						assessPrim := primitives.NewAssessBlindExecutionPrimitive(e.cfg.Get(), e.llm, baseSystem)
						decision, err := assessPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.AssessBlindInput{
							Step:   &step,
							Extras: extras,
						})
						if err == nil && decision.Decision == "evaluate" {
							shouldEvaluate = true
							extras = append(extras, "SOFT NUDGE: Agent elected to evaluate due to exceeding blind action threshold ("+decision.Reason+")")
						} else {
							blindActions = 0 // Reset threshold so it doesn't nudge every single action after
							if err == nil && decision.Decision == "continue" {
								e.log(state.LogSystem, "AssessBlindExecution", "background LLM elected to let the agent continue blind actions", decision.Reason, "")
							}
						}
					}
				}

				if !shouldEvaluate {
					continue
				}
				r, err := e.reflect(ctx, step, *d.Tool, res, extras)
				if err != nil {
					return "", "", err
				}
				if r.Status == "failed" {
					consecutiveFailures++
					if consecutiveFailures >= 3 {
						critPrim := primitives.NewCritiqueApproachPrimitive(e.cfg.Get(), e.llm, baseSystem)
						critique, err := critPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.CritiqueApproachInput{
							Purpose: "REPEATED FAILURES DETECTED. CRITIQUE REQUIRED.",
							Step:    &step,
							Extras:  extras,
						})
						if err == nil {
							extras = append(extras, "CRITIQUE OF FAILED APPROACH:\n"+mustJSON(critique))
						}
						consecutiveFailures = 0
					}
				} else if r.Status == "success" {
					consecutiveFailures = 0
				}
				outcome, why, err := e.handleReflection(ctx, r, &step, &extras, filesChanged)
				if e.cfg.Get().EvaluationMode == "Dynamic" {
					pinnedEvaluation = mustJSON(r)
					blindTrail = []string{}
					blindActions = 0
				}
				if err != nil {
					return "", "", err
				}
				if outcome == "replan" {
					return "replan", why, nil
				}
			}
		case "step_complete":
			if snap.ActiveTask.ComplexityMode == "Fast" {
				synPrim := primitives.NewSynthesizeTaskPrimitive(e.cfg.Get(), e.llm, baseSystem)
				synRes, err := synPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.SynthesizeTaskInput{
					Goal:             step.Goal,
					PinnedEvaluation: pinnedEvaluation,
					Extras:           extras,
				})
				if err != nil {
					return "", "", err
				}
				e.addAgentMessage(synRes.Response, false)
				summaryPurpose := "TASK:\n" + step.Goal + "\n\nRESULTS:\n" + mustJSON(compactStepResults(snap.ActiveTask.StepResults))
				summaryText := e.finalizeTaskMemory(ctx, snap, baseSystem, summaryPurpose)
				e.addMemory(ctx, state.MemoryTask, summaryText+"\n\nOutput:\n"+synRes.Response, nil, nil, state.MessageAgent)
				e.store.CompleteActiveTask(state.TaskCompleted)
				extras = []string{}
				return "fast_complete", "", nil
			}

			r, err := e.evaluateStepCompletion(ctx, step, d.CompletionSummary, extras)
			if err != nil {
				return "", "", err
			}
			outcome, why, err := e.handleReflection(ctx, r, &step, &extras, filesChanged)
			if err != nil {
				return "", "", err
			}
			if outcome == "replan" {
				return "replan", why, nil
			}
			if r.Status != "success" {
				continue
			}
			if err := e.completeStep(ctx, step, extras, filesRead, filesChanged, toolIDs, artifacts); err != nil {
				return "", "", err
			}
			extras = []string{} // Clear extras for the next step
			return "done", "", nil
		}
	}
	return "", "", errors.New("step exceeded maximum execution loops")
}

func (e *Engine) checkpointStep(ctx context.Context, step state.PlanStep, extras []string) error {
	genArtPrim := primitives.NewGenerateStepArtifactPrimitive(e.cfg.Get(), e.llm, baseSystem)
	genArt, err := genArtPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.GenerateStepArtifactInput{
		Step:   step,
		Extras: extras,
	})
	if err != nil {
		e.log(state.LogSystem, "GenerateStepArtifact", "failed to run artifact generation", "", err.Error())
	} else if genArt.ArtifactPayload != "" {
		snap := e.store.Snapshot()
		if snap.ActiveTask != nil {
			artName := fmt.Sprintf("step_%d_data.md", step.ID)
			artPath := filepath.Join(e.root, ".loa", "artifacts", snap.SessionID, fmt.Sprintf("task-%d", snap.ActiveTask.ID))
			os.MkdirAll(artPath, 0755)
			if err := os.WriteFile(filepath.Join(artPath, artName), []byte(genArt.ArtifactPayload), 0644); err == nil {
				extras = append(extras, "You automatically wrote dense execution data to artifact: "+artName)
			}
		}
	}

	extractPrim := primitives.NewExtractStepResultPrimitive(e.cfg.Get(), e.llm, baseSystem)
	ex, err := extractPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractStepResultInput{
		Step:   step,
		Extras: extras,
	})
	if err != nil {
		return err
	}
	e.store.UpdateActiveTask(func(t *state.TaskState) {
		t.Context.Facts = appendUnique(t.Context.Facts, ex.FactsLearned...)
		t.Context.Decisions = appendUnique(t.Context.Decisions, ex.Decisions...)
	})

	e.pruneMemory(ctx)
	return nil
}

func (e *Engine) pruneMemory(ctx context.Context) {
	prunePrim := primitives.NewPruneMemoryPrimitive(e.cfg.Get(), e.llm, baseSystem)
	snap := e.store.Snapshot()
	if len(snap.ActiveTask.Context.Facts) > 10 || len(snap.ActiveTask.Context.Decisions) > 10 {
		pruneRes, pruneErr := prunePrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.PruneMemoryInput{
			Facts:     snap.ActiveTask.Context.Facts,
			Decisions: snap.ActiveTask.Context.Decisions,
			Plan:      &snap.ActiveTask.Plan,
		})
		if pruneErr == nil {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				var newFacts []string
				for _, idx := range pruneRes.KeepFactIndices {
					if idx >= 0 && idx < len(t.Context.Facts) {
						newFacts = append(newFacts, t.Context.Facts[idx])
					}
				}
				var newDecisions []string
				for _, idx := range pruneRes.KeepDecisionIndices {
					if idx >= 0 && idx < len(t.Context.Decisions) {
						newDecisions = append(newDecisions, t.Context.Decisions[idx])
					}
				}
				t.Context.Facts = newFacts
				t.Context.Decisions = newDecisions
			})
			e.log(state.LogPlan, "PruneMemory", "pruned working memory", "", fmt.Sprintf("kept %d facts, %d decisions", len(pruneRes.KeepFactIndices), len(pruneRes.KeepDecisionIndices)))
		} else {
			e.log(state.LogPlan, "PruneMemory", "failed to prune memory", pruneErr.Error(), "")
		}
	}
}

func (e *Engine) getStep(id uint64) (state.PlanStep, error) {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return state.PlanStep{}, errors.New("no active task")
	}
	for _, s := range snap.ActiveTask.Plan.Steps {
		if s.ID == id {
			return s, nil
		}
	}
	return state.PlanStep{}, errors.New("plan step not found")
}

func (e *Engine) reflect(ctx context.Context, step state.PlanStep, req tools.Request, res state.ToolResult, extras []string) (primitives.ReflectionResult, error) {
	evalPrim := primitives.NewEvaluateStepResultPrimitive(e.cfg.Get(), e.llm, baseSystem)
	return evalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluateStepResultInput{
		Step:   step,
		Claim:  fmt.Sprintf("ACTION:\n%s\n\nRESULT:\n%s", mustJSON(requestToMap(req)), mustJSON(res)),
		Extras: extras,
	})
}
func (e *Engine) evaluateStepCompletion(ctx context.Context, step state.PlanStep, claim string, extras []string) (primitives.ReflectionResult, error) {
	evalPrim := primitives.NewEvaluateStepResultPrimitive(e.cfg.Get(), e.llm, baseSystem)
	return evalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluateStepResultInput{
		Step:   step,
		Claim:  claim,
		Extras: extras,
	})
}

func (e *Engine) handleReflection(ctx context.Context, r primitives.ReflectionResult, step *state.PlanStep, extras *[]string, filesChanged map[string]bool) (string, string, error) {
	*extras = append(*extras, "REFLECTION:\n"+mustJSON(r))
	switch r.Status {
	case "success", "partial", "verification_required":
		return "continue", "", nil
	case "needs_more_information":
		if r.SuggestedRecovery == "ask_user" {
			ans, err := e.askUser(ctx, r.Reason)
			if err != nil {
				return "", "", err
			}
			*extras = append(*extras, "USER CLARIFICATION: "+ans)
		}
		return "continue", "", nil
	case "plan_invalid":
		if e.store.Snapshot().ActiveTask.ComplexityMode == "Fast" {
			*extras = append(*extras, "CRITIQUE: Approach deemed invalid: "+r.Reason)
			return "continue", "", nil
		}
		return "replan", r.Reason, nil
	case "failed":

		purpose := "REFLECTION:\n" + mustJSON(r)
		routePrim := primitives.NewRouteNextPrimitive(e.cfg.Get(), e.llm, baseSystem)
		route, err := routePrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.RouteNextInput{
			Purpose:  purpose,
			StepJSON: mustJSON(step),
			Extras:   *extras,
		})
		if err != nil {
			return "", "", err
		}
		switch route.Action {
		case "rollback":
			if err := e.rollbackFiles(ctx, filesChanged); err != nil {
				*extras = append(*extras, "ROLLBACK FAILED: "+err.Error())
				return "continue", "", nil
			}
			if e.store.Snapshot().ActiveTask.ComplexityMode == "Fast" {
				*extras = append(*extras, "CRITIQUE: Approach failed, rollback executed. Reason: "+route.Reason)
				return "continue", "", nil
			}
			return "replan", route.Reason + " (Rollback executed)", nil
		case "replan":
			if e.store.Snapshot().ActiveTask.ComplexityMode == "Fast" {
				*extras = append(*extras, "CRITIQUE: Approach failed. Reason: "+route.Reason)
				return "continue", "", nil
			}
			return "replan", route.Reason, nil
		case "repair":
			if step.Mode != state.StepModify {
				return "replan", "Repair requested but current step is non-modifying (" + string(step.Mode) + "): " + route.Reason, nil
			}
			*extras = append(*extras, "RECOVERY ROUTE: "+mustJSON(route))
		case "ask_user":
			ans, err := e.askUser(ctx, coalesce(route.MissingInformation, route.Reason))
			if err != nil {
				return "", "", err
			}
			*extras = append(*extras, "USER CLARIFICATION: "+ans)
		default:
			*extras = append(*extras, "RECOVERY ROUTE: "+mustJSON(route))
		}
		return "continue", "", nil
	default:
		return "", "", fmt.Errorf("unhandled reflection status %q", r.Status)
	}
}

func (e *Engine) rollbackFiles(ctx context.Context, files map[string]bool) error {
	if len(files) == 0 {
		return nil
	}
	var args []string
	args = append(args, "checkout", "HEAD", "--")
	for f := range files {
		args = append(args, f)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = e.root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git checkout failed: %v, output: %s", err, string(out))
	}
	return nil
}

func (e *Engine) completeStep(ctx context.Context, step state.PlanStep, extras []string, filesRead, filesChanged map[string]bool, toolIDs []uint64, artifacts []string) error {
	genArtPrim := primitives.NewGenerateStepArtifactPrimitive(e.cfg.Get(), e.llm, baseSystem)
	genArt, err := genArtPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.GenerateStepArtifactInput{
		Step:   step,
		Extras: extras,
	})
	if err != nil {
		e.log(state.LogSystem, "GenerateStepArtifact", "failed to run artifact generation", "", err.Error())
	} else if genArt.ArtifactPayload != "" {
		snap := e.store.Snapshot()
		if snap.ActiveTask != nil {
			artName := fmt.Sprintf("step_%d_data.md", step.ID)
			artPath := filepath.Join(e.root, ".loa", "artifacts", snap.SessionID, fmt.Sprintf("task-%d", snap.ActiveTask.ID))
			os.MkdirAll(artPath, 0755)
			if err := os.WriteFile(filepath.Join(artPath, artName), []byte(genArt.ArtifactPayload), 0644); err == nil {
				extras = append(extras, "You automatically wrote dense execution data to artifact: "+artName)
				artifacts = appendUnique(artifacts, artName)
			}
		}
	}

	extractPrim := primitives.NewExtractStepResultPrimitive(e.cfg.Get(), e.llm, baseSystem)
	ex, err := extractPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractStepResultInput{
		Step:   step,
		Extras: extras,
	})
	if err != nil {
		return err
	}
	result := state.StepResult{StepID: step.ID, Success: true, Summary: ex.Summary, FactsLearned: ex.FactsLearned, Decisions: ex.Decisions, FilesRead: keys(filesRead), FilesChanged: keys(filesChanged), Artifacts: artifacts, ToolCallIDs: toolIDs, CreatedAt: time.Now()}

	snap := e.store.Snapshot()
	refreshed := state.TaskContext{}
	refreshOK := false
	if snap.ActiveTask != nil {
		purpose := "PREVIOUS TASK CONTEXT:\n" + mustJSON(snap.ActiveTask.Context) + "\n\nNEW COMPLETED STEP RESULT:\n" + mustJSON(result)
		refreshPrim := primitives.NewRefreshTaskContextPrimitive(e.cfg.Get(), e.llm, baseSystem)
		var err error
		refreshed, err = refreshPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.RefreshTaskContextInput{
			Purpose: purpose,
			Step:    &step,
		})
		if err != nil {
			e.log(state.LogSystem, "RefreshTaskContext", "task context refresh failed; using deterministic fallback", purpose, err.Error())
		} else {
			refreshOK = true
		}
	}

	e.store.UpdateActiveTask(func(t *state.TaskState) {
		t.StepResults = append(t.StepResults, result)
		if refreshOK {
			t.Context = refreshed
		} else {
			t.Context.Facts = appendUnique(t.Context.Facts, ex.FactsLearned...)
			t.Context.Decisions = appendUnique(t.Context.Decisions, ex.Decisions...)
		}
		for i := range t.Plan.Steps {
			if t.Plan.Steps[i].ID == step.ID {
				t.Plan.Steps[i].Status = state.StepCompleted
			}
		}
	})
	if ex.Summary != "" {
		e.addMemory(ctx, state.MemoryStepResult, ex.Summary, nil, &step.ID, state.MessageAgent)
	}
	e.pruneMemory(ctx)
	return nil
}

func (e *Engine) runIterativeEvaluation(ctx context.Context, isReadOnly bool) (primitives.FinalCheck, error) {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return primitives.FinalCheck{}, errors.New("no active task")
	}
	if len(snap.ActiveTask.AcceptanceCriteria) == 0 {
		if err := e.updateAcceptanceCriteria(ctx, snap.ActiveTask.Goal, "", nil); err != nil {
			return primitives.FinalCheck{}, err
		}
		snap = e.store.Snapshot()
	}

	purpose := fmt.Sprintf("ORIGINAL TASK:\n%s\n\nPRIOR VERIFICATION CONCERNS:\n%s", snap.ActiveTask.Goal, mustJSON(compactVerificationConcerns(snap)))

	checklist := make([]state.AcceptanceCheck, len(snap.ActiveTask.AcceptanceCriteria))
	for i, c := range snap.ActiveTask.AcceptanceCriteria {
		checklist[i] = state.AcceptanceCheck{
			CriterionID: c.ID,
			Status:      state.AcceptanceUnverified,
		}
	}

	var extras []string
	evalPrim := primitives.NewIterativeEvaluationPrimitive(e.cfg.Get(), e.llm, baseSystem)

	// We'll allow up to MaxPlanDepth iterations for the auditor to verify things.
	maxLoops := e.cfg.Get().MaxPlanDepth
	if maxLoops <= 0 {
		maxLoops = 10
	}

	for loop := 0; loop < maxLoops; loop++ {
		cb := &engineContextBuilder{e: e}

		res, err := evalPrim.Execute(ctx, cb, primitives.IterativeEvaluationInput{
			Purpose:    purpose,
			Checklist:  checklist,
			IsReadOnly: isReadOnly,
		})
		if err != nil {
			return primitives.FinalCheck{}, err
		}

		checklist = res.Checklist

		if res.Action == "complete" {
			e.store.UpdateActiveTask(func(t *state.TaskState) { t.AcceptanceChecks = append([]state.AcceptanceCheck(nil), checklist...) })
			return primitives.FinalCheck{Complete: true, Checks: checklist}, nil
		} else if res.Action == "failed" {
			e.store.UpdateActiveTask(func(t *state.TaskState) { t.AcceptanceChecks = append([]state.AcceptanceCheck(nil), checklist...) })
			return primitives.FinalCheck{Complete: false, Reason: res.Reason, Checks: checklist}, nil
		} else if res.Action == "tool_call" && res.Tool != nil {
			// Strict check: auditor may only use read-only tools.
			if tools.IsMutating(res.Tool.Kind) {
				extras = append(extras, "ERROR: Iterative Auditor is not allowed to use mutating tools like "+string(res.Tool.Kind))
				continue
			}

			toolRes, err := e.executeTool(ctx, *res.Tool, false)
			if err != nil {
				extras = append(extras, fmt.Sprintf("Tool Execution Error: %v", err))
			} else {
				extras = append(extras, formatToolResult(*res.Tool, toolRes))
			}
		}
	}

	e.store.UpdateActiveTask(func(t *state.TaskState) { t.AcceptanceChecks = append([]state.AcceptanceCheck(nil), checklist...) })
	return primitives.FinalCheck{Complete: false, Reason: "Iterative Auditor exceeded maximum loops without reaching a complete or failed verdict.", Checks: checklist}, nil
}

func (e *Engine) finishTask(ctx context.Context, check primitives.FinalCheck) error {
	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		return errors.New("no active task")
	}

	var synthStep *state.PlanStep
	for i := range snap.ActiveTask.Plan.Steps {
		if snap.ActiveTask.Plan.Steps[i].Mode == state.StepSynthesize {
			synthStep = &snap.ActiveTask.Plan.Steps[i]
			break
		}
	}

	if synthStep == nil {
		newID := e.store.NextID()
		step := state.PlanStep{
			ID:             newID,
			Title:          "Final Synthesis",
			Goal:           "Review all step artifacts and memories. Composite a highly detailed, comprehensive final task report. Write it using artifact_write to final_response.md.",
			Reason:         "Need to compose the final task response from step data.",
			ExpectedResult: "A final_response.md artifact.",
			Mode:           state.StepSynthesize,
			Status:         state.StepPending,
			ExpectedTools:  []state.ToolKind{state.ToolSearchTaskSteps, state.ToolReadTaskStep, state.ToolArtifactRead, state.ToolArtifactWrite, state.ToolArtifactAppend, state.ToolArtifactPatch},
		}
		e.store.UpdateActiveTask(func(t *state.TaskState) {
			t.Plan.Steps = append(t.Plan.Steps, step)
		})
		return errors.New("continue_synthesis")
	}

	if synthStep.Status != state.StepCompleted {
		return errors.New("continue_synthesis")
	}

	artPath := filepath.Join(e.root, ".loa", "artifacts", snap.SessionID, fmt.Sprintf("task-%d", snap.ActiveTask.ID), "final_response.md")
	b, err := os.ReadFile(artPath)
	var answer string
	if err == nil {
		answer = string(b)
	} else {
		e.log(state.LogSystem, "FinishTask", "failed to read final_response.md", "", err.Error())
		answer = "Task complete, but final_response.md was not generated successfully."
	}

	goal := snap.ActiveTask.Goal
	summaryPurpose := "TASK:\n" + goal
	summaryText := e.finalizeTaskMemory(ctx, snap, baseSystem, summaryPurpose)

	e.store.CompleteActiveTask(state.TaskCompleted)

	e.addAgentMessage(answer, false)
	e.addMemory(ctx, state.MemoryTask, summaryText+"\n\nOutput:\n"+answer, nil, nil, state.MessageAgent)

	if err := e.store.Save("autosave"); err != nil {
		return fmt.Errorf("auto-save completed task: %w", err)
	}
	return nil
}

func (e *Engine) finalizeTaskMemory(ctx context.Context, snap state.AgentState, baseSystem string, summaryPurpose string) string {
	sumPrim := primitives.NewSummarizeTaskPrimitive(e.cfg.Get(), e.llm, baseSystem)
	summary, err := sumPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.SummarizeTaskInput{
		Purpose: summaryPurpose,
	})
	if err == nil && strings.TrimSpace(summary.Summary) == "" {
		err = errors.New("summary required")
	}
	if err != nil {
		e.log(state.LogSystem, "SummarizeTask", "task summary generation failed; using deterministic fallback", summaryPurpose, err.Error())
		summary.Summary = fallbackTaskSummary(snap.ActiveTask.StepResults)
	}

	extractKwPrim := primitives.NewExtractKeywordsPrimitive(e.cfg.Get(), e.llm, baseSystem)
	ext, err := extractKwPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractKeywordsInput{Text: summary.Summary})
	if err != nil {
		e.log(state.LogSystem, "ExtractKeywords", "keyword extraction failed for summary", summary.Summary, err.Error())
	}

	embedText := summary.Summary
	if len(ext.IndexKeys) > 0 {
		embedText = strings.Join(ext.IndexKeys, " ")
	}

	emb, _ := e.tryEmbed(ctx, embedText)
	summaryItem := state.TaskSummary{TaskID: snap.ActiveTask.ID, Summary: summary.Summary, IndexKeys: ext.IndexKeys, Embedding: emb, CreatedAt: time.Now()}
	if sStore := e.SessionMemStore(); sStore != nil {
		sStore.AddSummary(summaryItem)
	} else {
		e.memStore.AddSummary(summaryItem)
	}

	if len(snap.ActiveTask.Context.Facts) > 0 || len(snap.ActiveTask.Context.Decisions) > 0 {
		purpose := "WORKING MEMORY TO CONSOLIDATE:\n\nFacts:\n"
		for _, f := range snap.ActiveTask.Context.Facts {
			purpose += "- " + f + "\n"
		}
		purpose += "\nDecisions:\n"
		for _, d := range snap.ActiveTask.Context.Decisions {
			purpose += "- " + d + "\n"
		}

		prim := primitives.NewConsolidateContextPrimitive(e.cfg.Get(), e.llm, baseSystem)
		result, err := prim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ConsolidateContextInput{
			Purpose: purpose,
		})

		if err == nil {
			for _, item := range result.Items {
				if item.Classification == primitives.ConsolidationSemanticFact {
					e.addMemory(ctx, state.MemoryFact, item.Text, nil, nil, state.MessageAgent)
				} else if item.Classification == primitives.ConsolidationArchitecturalDecision {
					e.addMemory(ctx, state.MemoryDecision, item.Text, nil, nil, state.MessageAgent)
				}
			}
		} else {
			e.log(state.LogSystem, "ConsolidateContext", "memory consolidation failed, dropping memory to prevent bloat", "", err.Error())
		}
	}
	
	return summary.Summary
}

func fallbackTaskSummary(results []state.StepResult) string {
	if len(results) == 0 {
		return "The task completed, but no step summary was available."
	}
	parts := make([]string, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.Summary) != "" {
			parts = append(parts, strings.TrimSpace(result.Summary))
		}
	}
	if len(parts) == 0 {
		return "The task completed and its plan steps were recorded."
	}
	if len(parts) > 3 {
		parts = parts[len(parts)-3:]
	}
	return strings.Join(parts, " ")
}

func (e *Engine) generateFinalResponse(ctx context.Context, userText, proposed string, step *state.PlanStep, extras []string) (string, error) {
	snap := e.store.Snapshot()
	req := primitives.PromptRequest{
		PrimitiveInstructions: "Generate the final user-facing response. Be concise and factual. Do not claim work not evidenced.",
		PrimitiveInputLabel:   "\nUSER REQUEST:\n",
		PrimitiveInput:        userText + "\n\nPROPOSED CONTENT / CONCLUSION:\n" + proposed,
		Step:                  step,
		Extras:                extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	prompt, err := buildPrompt(e.root, e.cfg.Get(), snap, finalResponseSystem, req)
	if err != nil {
		e.log(state.LogSystem, "GenerateFinalResponse", "budget constraint violated", "", err.Error())
	}
	e.setLastContext(prompt)
	e.recordInference("GenerateFinalResponse", 1)
	out, err := e.llm.ChatText(ctx, e.cfg.Get().ModelExecuting, finalResponseSystem, prompt)
	e.log(state.LogLLM, "GenerateFinalResponse", "final response", prompt, out)
	return strings.TrimSpace(out), err
}

func (e *Engine) AddGlobalConcept(ctx context.Context, text string) {
	e.addMemory(ctx, state.MemoryGlobalConcept, text, nil, nil, state.MessageUser)
}


func (e *Engine) log(kind state.LogKind, primitive, summary, input, output string) {
	snap := e.store.Snapshot()
	var taskID, stepID *uint64
	if snap.ActiveTask != nil {
		id := snap.ActiveTask.ID
		taskID = &id
		for _, s := range snap.ActiveTask.Plan.Steps {
			if s.Status == state.StepRunning {
				x := s.ID
				stepID = &x
				break
			}
		}
	}

	var cat state.LogCategory
	switch primitive {
	case "ExecuteStep", "tool":
		cat = state.CategoryTool
	case "ConsolidateContext", "RetrieveMemory", "ExtractKeywords":
		cat = state.CategoryMemoryLookup
	case "Reindex", "crawler":
		cat = state.CategoryIndexing
	case "Negotiation":
		cat = state.CategoryNegotiation
	default:
		if kind == state.LogTool {
			cat = state.CategoryTool
		} else if kind == state.LogPlan {
			cat = state.CategoryPlan
		} else {
			cat = state.CategorySystem
		}
	}

	e.store.AddLog(state.ExecutionLogEntry{ID: e.store.NextID(), Kind: kind, Category: cat, TaskID: taskID, StepID: stepID, Primitive: primitive, Summary: summary, Input: input, RawOutput: output, CreatedAt: time.Now()})
}
func (e *Engine) setLastContext(s string) { e.mu.Lock(); e.lastContext = s; e.mu.Unlock() }
func (e *Engine) recordInference(primitive string, n uint64) {
	e.mu.Lock()
	e.inferenceByPrimitive[primitive] += n
	e.mu.Unlock()
}
func (e *Engine) ResetSessionView() {
	e.mu.Lock()
	e.lastContext = ""
	e.lastError = ""
	e.mu.Unlock()
}
func (e *Engine) addAgentMessage(text string, clarification bool) {
	e.store.AddMessage(state.Message{ID: e.store.NextID(), Source: state.MessageAgent, Text: text, CreatedAt: time.Now(), Clarification: clarification})
}

func mustJSON(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) }
func coalesce(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
func appendUnique(dst []string, vals ...string) []string {
	seen := map[string]bool{}
	for _, x := range dst {
		seen[x] = true
	}
	for _, x := range vals {
		if x != "" && !seen[x] {
			dst = append(dst, x)
			seen[x] = true
		}
	}
	return dst
}

func formatPlanForMemory(plan state.Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan Version %d:\n", plan.Version)
	for _, s := range plan.Steps {
		fmt.Fprintf(&b, "- Step %d: %s (Goal: %s)\n", s.ID, s.Title, s.Goal)
	}
	return b.String()
}
