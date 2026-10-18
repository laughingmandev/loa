package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/embedder"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

func (e *Engine) executeTool(ctx context.Context, req tools.Request, mayModify bool) (state.ToolResult, error) {
	if e.hasPendingIntervention() {
		return state.ToolResult{Success: false, Error: "intervention_superseded", CreatedAt: time.Now()}, nil
	}
	if tools.IsMutating(req.Kind) && req.Kind != state.ToolExecuteProcess && req.Kind != state.ToolExecuteShell && !mayModify {
		return state.ToolResult{}, fmt.Errorf("tool %s is not allowed in non-modifying mode", req.Kind)
	}
	var input any
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &input); err != nil {
			return state.ToolResult{}, fmt.Errorf("invalid tool input: %w", err)
		}
	}
	call := state.ToolCall{ID: e.store.NextID(), Kind: req.Kind, Description: req.Description, Input: input, CreatedAt: time.Now()}
	call.RequiresApproval = e.requiresApproval(req.Kind)
	if !mayModify && (req.Kind == state.ToolExecuteProcess || req.Kind == state.ToolExecuteShell) {
		if e.cfg.Get().Permissions.Mode != config.PermissionAllowAll {
			call.RequiresApproval = true
		}
	}
	runKey := e.commandApprovalKey(call)
	call.AllowRunEligible = runKey != ""
	if !mayModify && e.cfg.Get().Permissions.Mode != config.PermissionAllowAll {
		call.AllowRunEligible = false
	}
	if call.RequiresApproval && call.AllowRunEligible && e.runApprovalAllowed(runKey) {
		approved := true
		call.Approved = &approved
	}
	e.store.AddToolCall(call)
	e.log(state.LogTool, "ToolRequest", fmt.Sprintf("requested %s", req.Kind), mustJSON(call), "")
	if call.RequiresApproval && call.Approved != nil && *call.Approved {
		e.log(state.LogPermission, "PermissionGate", "exact command auto-approved for current Loa run", mustJSON(call), "allow_run")
	} else if call.RequiresApproval {
		ch := make(chan approvalResolution, 1)
		approvalRunKey := ""
		if call.AllowRunEligible {
			approvalRunKey = runKey
		}
		e.mu.Lock()
		e.approvals[call.ID] = approvalRequest{ch: ch, runKey: approvalRunKey}
		e.mu.Unlock()
		e.log(state.LogPermission, "PermissionGate", "waiting for user approval", mustJSON(call), "")
		var resolution approvalResolution
		select {
		case resolution = <-ch:
		case <-ctx.Done():
			e.mu.Lock()
			delete(e.approvals, call.ID)
			e.mu.Unlock()
			return state.ToolResult{}, ctx.Err()
		}
		e.mu.Lock()
		delete(e.approvals, call.ID)
		e.mu.Unlock()
		if resolution.superseded {
			e.store.SetToolApproval(call.ID, false)
			res := state.ToolResult{ToolCallID: call.ID, Success: false, Error: "intervention_superseded", CreatedAt: time.Now()}
			e.store.AddToolResult(res)
			e.log(state.LogPermission, "PermissionGate", "pending tool action superseded by user guidance", mustJSON(call), mustJSON(res))
			return res, nil
		}
		if !resolution.approved {
			res := state.ToolResult{ToolCallID: call.ID, Success: false, Error: "permission_denied", CreatedAt: time.Now()}
			e.store.AddToolResult(res)
			e.log(state.LogPermission, "PermissionGate", "user denied tool action", mustJSON(call), mustJSON(res))
			return res, nil
		}
		e.log(state.LogPermission, "PermissionGate", "user approved tool action", mustJSON(call), "approved")
	}
	if call.Kind == state.ToolSearchTaskSteps {
		res := e.handleSearchTaskSteps(call)
		e.store.AddToolResult(res)
		e.log(state.LogTool, "ToolResult", fmt.Sprintf("%s completed (success=%v)", req.Kind, res.Success), mustJSON(call), mustJSON(res))
		return res, nil
	}
	if call.Kind == state.ToolReadTaskStep {
		res := e.handleReadTaskStep(call)
		e.store.AddToolResult(res)
		e.log(state.LogTool, "ToolResult", fmt.Sprintf("%s completed (success=%v)", req.Kind, res.Success), mustJSON(call), mustJSON(res))
		return res, nil
	}
	var activeTaskID uint64
	if snap := e.store.Snapshot(); snap.ActiveTask != nil {
		activeTaskID = snap.ActiveTask.ID
	}
	res := e.tools.Execute(ctx, call, activeTaskID)
	
	limit := e.cfg.Get().MaxPromptToolOutputBytes
	if limit <= 0 {
		limit = 30000
	}
	if len(res.Output) > limit {
		half := limit / 2
		artName := fmt.Sprintf("spill_%d.log", time.Now().UnixNano())
		_ = e.tools.DumpToArtifact(activeTaskID, artName, res.Output)
		truncMsg := fmt.Sprintf("\n\n... [TRUNCATED - EXCEEDS PROMPT LIMIT (%d > %d bytes). FULL OUTPUT SAVED TO ARTIFACT: %s. Use artifact_read to read it.] ...\n\n", len(res.Output), limit, artName)
		res.Output = res.Output[:half] + truncMsg + res.Output[len(res.Output)-half:]
		
		if activeTaskID != 0 {
			e.store.UpdateActiveTask(func(t *state.TaskState) {
				found := false
				for _, a := range t.Context.Artifacts {
					if a == artName {
						found = true
						break
					}
				}
				if !found {
					t.Context.Artifacts = append(t.Context.Artifacts, artName)
				}
			})
		}
	}

	e.store.AddToolResult(res)
	e.log(state.LogTool, "ToolResult", fmt.Sprintf("%s completed (success=%v)", req.Kind, res.Success), mustJSON(call), mustJSON(res))
	return res, nil
}

func (e *Engine) commandApprovalKey(call state.ToolCall) string {
	if call.Kind != state.ToolExecuteProcess && call.Kind != state.ToolExecuteShell {
		return ""
	}
	b, err := json.Marshal(call.Input)
	if err != nil {
		return ""
	}
	return e.root + "\x00" + string(call.Kind) + "\x00" + string(b)
}

func (e *Engine) runApprovalAllowed(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.runAllowed[key]
	return ok
}

func (e *Engine) requiresApproval(kind state.ToolKind) bool {
	p := e.cfg.Get().Permissions
	switch p.Mode {
	case config.PermissionAskAll:
		return true
	case config.PermissionAllowAll:
		return false
	case config.PermissionSelected:
		return p.AskFor[kind]
	default:
		return true
	}
}

func toolSignature(req tools.Request) string {
	var input any
	_ = json.Unmarshal(req.Input, &input)
	b, _ := json.Marshal(input)
	return string(req.Kind) + "|" + string(b)
}

func formatToolResult(req tools.Request, res state.ToolResult) string {
	exit := "<nil>"
	if res.ExitCode != nil {
		exit = fmt.Sprintf("%d", *res.ExitCode)
	}
	return fmt.Sprintf("[ACTION EXECUTED - AWAITING EVALUATION]\nTOOL %s (%s)\nINPUT: %s\nSUCCESS: %v\nEXIT: %s\nERROR: %s\nOUTPUT:\n%s", req.Kind, req.Description, string(req.Input), res.Success, exit, res.Error, res.Output)
}
func trackToolFiles(req tools.Request, res state.ToolResult, reads, changes map[string]bool) {
	var x struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(req.Input, &x)
	if x.Path == "" {
		return
	}
	switch req.Kind {
	case state.ToolReadFile, state.ToolReadRange:
		reads[x.Path] = true
	case state.ToolWriteFile, state.ToolPatchFile, state.ToolPatchASTNode, state.ToolDeleteFile:
		changes[x.Path] = true
	}
}

func (e *Engine) askUser(ctx context.Context, missing string) (string, error) {
	followup := missing
	base := e.store.Snapshot()
	var taskID, stepID *uint64
	originalStatus := state.TaskRunning
	if base.ActiveTask != nil {
		id := base.ActiveTask.ID
		taskID = &id
		originalStatus = base.ActiveTask.Status
		for _, s := range base.ActiveTask.Plan.Steps {
			if s.Status == state.StepRunning {
				x := s.ID
				stepID = &x
				break
			}
		}
	}
	for {
		askPrim := primitives.NewAskUserPrimitive(e.cfg.Get(), e.llm, baseSystem)
		q, err := askPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.AskUserInput{
			Purpose: "INFORMATION NEEDED:\n" + followup,
		})
		if err != nil {
			return "", err
		}
		if taskID != nil {
			e.store.UpdateActiveTask(func(t *state.TaskState) { t.Status = state.TaskWaiting })
		}
		qid := e.store.NextID()
		pq := &state.PendingQuestion{ID: qid, TaskID: taskID, StepID: stepID, Question: q.Question, Reason: followup, ResumePrimitive: "EvaluateUserAnswer", AskedAt: time.Now()}
		e.store.SetPendingQuestion(pq)
		e.addAgentMessage(q.Question, true)
		ch := make(chan state.Message, 1)
		e.mu.Lock()
		e.questionCh = ch
		e.mu.Unlock()
		var reply state.Message
		select {
		case reply = <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		e.mu.Lock()
		if e.questionCh == ch {
			e.questionCh = nil
		}
		e.mu.Unlock()
		intent, err := e.interpretIntent(ctx, reply.Text)
		if err != nil {
			return "", err
		}
		e.store.SetMessageIntents(reply.ID, intent.Intents)
		_ = e.extractDurableMemory(ctx, []uint64{reply.ID})
		purpose := fmt.Sprintf("QUESTION:\n%s\n\nUSER ANSWER:\n%s", q.Question, reply.Text)
		evalPrim := primitives.NewEvaluateUserAnswerPrimitive(e.cfg.Get(), e.llm, baseSystem)
		ev, err := evalPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.EvaluateUserAnswerInput{
			Purpose: purpose,
		})
		if err != nil {
			return "", err
		}
		if ev.Sufficient {
			e.store.SetPendingQuestion(nil)
			if taskID != nil {
				e.store.UpdateActiveTask(func(t *state.TaskState) { t.Status = originalStatus })
			}
			return reply.Text, nil
		}
		followup = coalesce(ev.FollowupNeeded, ev.Reason)
	}
}

func (e *Engine) extractDurableMemory(ctx context.Context, sourceMessageIDs []uint64) error {
	snap := e.store.Snapshot()
	recent := tailMessages(snap.Messages, 6)
	extPrim := primitives.NewExtractDurableMemoryPrimitive(e.cfg.Get(), e.llm, baseSystem)
	out, err := extPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractDurableMemoryInput{
		Purpose: "RECENT INTERACTION:\n" + mustJSON(recent),
	})
	if err != nil {
		return err
	}
	for _, m := range out.Items {
		e.addMemory(ctx, m.Kind, m.Text, sourceMessageIDs, nil, m.Participant)
	}
	return nil
}

func (e *Engine) addMemory(ctx context.Context, kind state.MemoryKind, text string, sourceMessages []uint64, stepID *uint64, participant state.MessageSource) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	extPrim := primitives.NewExtractKeywordsPrimitive(e.cfg.Get(), e.llm, baseSystem)
	ext, err := extPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractKeywordsInput{Text: text})
	if err != nil {
		e.log(state.LogSystem, "ExtractKeywords", "keyword extraction failed", text, err.Error())
	}

	embedText := text
	if len(ext.IndexKeys) > 0 {
		embedText = strings.Join(ext.IndexKeys, " ")
	}

	emb, _ := e.tryEmbed(ctx, embedText)
	snap := e.store.Snapshot()
	var taskID *uint64
	if snap.ActiveTask != nil {
		id := snap.ActiveTask.ID
		taskID = &id
	}
	mem := state.MemoryItem{ID: 0, Kind: kind, Text: text, Embedding: emb, IndexKeys: ext.IndexKeys, SourceMessageIDs: sourceMessages, Participant: participant, SourceTaskID: taskID, SourceStepID: stepID, CreatedAt: time.Now()}
	if kind == state.MemoryStructural || kind == state.MemoryGlobalConcept {
		e.memStore.AddMemory(mem)
	} else {
		if sStore := e.SessionMemStore(); sStore != nil {
			sStore.AddMemory(mem)
		} else {
			e.memStore.AddMemory(mem)
		}
	}
}
func (e *Engine) tryEmbed(ctx context.Context, text string) ([]float32, error) {
	if e.cfg.Get().EmbeddingEngine == "local" {
		e.mu.Lock()
		path := e.cfg.Get().LocalEmbeddingModelPath
		if path != "" && path != e.loadedLocalEmbeddingModelPath {
			if e.localEmbedder != nil {
				e.localEmbedder.Close()
				e.localEmbedder = nil
			}
			emb, err := embedder.NewLocalEmbedder(path)
			if err == nil {
				e.localEmbedder = emb
				e.loadedLocalEmbeddingModelPath = path
			} else {
				e.log(state.LogSystem, "Init", "lazy loading local embedder failed", path, err.Error())
			}
		}
		e.mu.Unlock()

		if e.localEmbedder != nil {
			emb, err := e.localEmbedder.Embed(text)
			if err == nil {
				e.localEmbeddings.Add(1)
			}
			if err != nil {
				e.log(state.LogSystem, "Embedding", "local embedding request failed", text, err.Error())
			}
			return emb, err
		}
		return nil, errors.New("local embedding model is not loaded (check configuration)")
	}

	if e.cfg.Get().EmbeddingModel == "" {
		return nil, errors.New("no embedding model selected")
	}
	emb, err := e.llm.Embed(ctx, text)
	if err != nil {
		e.log(state.LogSystem, "Embedding", "openapi embedding request failed", text, err.Error())
	}
	return emb, err
}

func (e *Engine) retrieveMemory(ctx context.Context, query string, step *state.PlanStep) (string, error) {
	return e.retrieveMemoryRound(ctx, query, nil, step, 0)
}

func (e *Engine) prefetchDiscussionMemory(ctx context.Context, text string, step *state.PlanStep) (string, error) {
	extPrim := primitives.NewExtractKeywordsPrimitive(e.cfg.Get(), e.llm, baseSystem)
	ext, err := extPrim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ExtractKeywordsInput{Text: text})
	if err != nil {
		return "", err
	}
	if len(ext.IndexKeys) == 0 {
		return "", nil
	}
	query := strings.Join(ext.IndexKeys, " ")
	return e.retrieveMemoryRound(ctx, query, nil, step, 0)
}

func (e *Engine) retrieveMemoryRound(ctx context.Context, query string, allowedKinds []string, step *state.PlanStep, round int) (string, error) {
	if e.hasStaleFiles() {
		if err := e.repairStaleFiles(ctx); err != nil {
			e.log(state.LogSystem, "Watcher", "failed to repair stale files before memory retrieval", "", err.Error())
		}
	}
	emb, err := e.tryEmbed(ctx, query)
	if err != nil {
		return "", err
	}
	
	var kinds []state.MemoryKind
	for _, k := range allowedKinds {
		kinds = append(kinds, state.MemoryKind(k))
	}

	cfg := e.cfg.Get()
	pool := cfg.MemoryCandidatePool * (round + 1)
	if pool > cfg.MemoryCandidatePool*cfg.MemoryPoolExpansionLimit {
		pool = cfg.MemoryCandidatePool * cfg.MemoryPoolExpansionLimit
	}
	var targets []string
	if step != nil {
		targets = step.TargetEntities
	}
	cands := e.memStore.Search(emb, pool, time.Now(), targets, kinds)
	if sStore := e.SessionMemStore(); sStore != nil {
		sCands := sStore.Search(emb, pool, time.Now(), targets, kinds)
		cands = append(cands, sCands...)
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].Score > cands[j].Score })
		if len(cands) > pool {
			cands = cands[:pool]
		}
	}
	e.log(state.LogSystem, "SemanticSearch", fmt.Sprintf("Top-K vector search returned %d candidates", len(cands)), query, mustJSON(cands))
	if len(cands) <= 1 {
		if len(cands) == 0 {
			return "", nil
		}
		cand := cands[0]
		if cand.Kind == state.MemoryStructural {
			return "STRUCTURAL CODE REFERENCES:\n" + mustJSON(cands), nil
		}
		return "SYNTHESIZED SESSION CONTEXT:\n" + cand.Text, nil
	}
	clarification := ""
	for gate := 0; gate < cfg.MemoryRerankGates; gate++ {
		var rr primitives.RerankResult
		validIDs := make(map[uint64]bool, len(cands))
		for _, cand := range cands {
			validIDs[cand.ID] = true
		}
		purpose := fmt.Sprintf("QUERY:\n%s\n\nMAX SELECT: %d\n\nCANDIDATES (select by stable id, with timestamps):\n%s", query, cfg.MemoryTopK, mustJSON(cands))
		if clarification != "" {
			purpose += "\n\nUSER CLARIFICATION RECEIVED:\n" + clarification
		}
		rerankPrim := primitives.NewRerankMemoryPrimitive(cfg, e.llm, baseSystem)
		rr, err = rerankPrim.Execute(ctx, &engineContextBuilder{e: e, }, primitives.RerankMemoryInput{
			Purpose:  purpose,
			Step:     step,
			ValidIDs: validIDs,
			Limit:    cfg.MemoryTopK,
		})
		if err != nil {
			e.log(state.LogSystem, "RerankMemory", "memory relevance/conflict gate failed; using similarity order", query, err.Error())
			limit := len(cands)
			if limit > cfg.MemoryTopK {
				limit = cfg.MemoryTopK
			}
			var structCands []memory.Candidate
			var semCands []string
			for _, cand := range cands[:limit] {
				if cand.Kind == state.MemoryStructural {
					structCands = append(structCands, cand)
				} else {
					semCands = append(semCands, cand.Text)
				}
			}
			result := ""
			if len(semCands) > 0 {
				result += "SYNTHESIZED SESSION CONTEXT (Fallback - Unmerged):\n" + strings.Join(semCands, "\n") + "\n\n"
			}
			if len(structCands) > 0 {
				result += "STRUCTURAL CODE REFERENCES:\n" + mustJSON(structCands)
			}
			return strings.TrimSpace(result), nil
		}
		if rr.NeedsUser {
			ans, askErr := e.askUser(ctx, rr.ClarificationNeeded)
			if askErr != nil {
				return "", askErr
			}
			clarification = ans
			continue
		}
		byID := make(map[uint64]memory.Candidate, len(cands))
		for _, cand := range cands {
			byID[cand.ID] = cand
		}
		out := make([]memory.Candidate, 0, len(rr.SelectedStructuralIDs))
		for _, id := range rr.SelectedStructuralIDs {
			out = append(out, byID[id])
		}
		
		result := ""
		if rr.SynthesizedContext != "" {
			result += "SYNTHESIZED SESSION CONTEXT:\n" + rr.SynthesizedContext + "\n\n"
		}
		if len(out) > 0 {
			result += "STRUCTURAL CODE REFERENCES:\n" + mustJSON(out)
		}
		
		return strings.TrimSpace(result), nil
	}
	return "", errors.New("memory conflict remained unresolved after clarification attempts")
}

func (e *Engine) taskSummaryText() string {
	var ss []state.TaskSummary
	if sStore := e.SessionMemStore(); sStore != nil {
		ss = sStore.Summaries()
	} else {
		ss = e.memStore.Summaries()
	}
	if len(ss) > 50 {
		ss = ss[len(ss)-50:]
	}
	return "PAST TASK SUMMARIES:\n" + mustJSON(ss)
}

func requestToMap(r tools.Request) any {
	var input any
	if len(r.Input) > 0 {
		_ = json.Unmarshal(r.Input, &input)
	}
	return map[string]any{"kind": r.Kind, "description": r.Description, "input": input}
}

func (e *Engine) DebugDump(includeEmbeddings bool) ([]byte, error) {
	snap := e.store.Snapshot()
	memItems := e.memStore.Items()
	var summaries []state.TaskSummary
	if sStore := e.SessionMemStore(); sStore != nil {
		memItems = append(memItems, sStore.Items()...)
		summaries = sStore.Summaries()
	} else {
		summaries = e.memStore.Summaries()
	}
	if !includeEmbeddings {
		for i := range memItems {
			memItems[i].Embedding = nil
		}
		for i := range summaries {
			summaries[i].Embedding = nil
		}
	}
	dump := map[string]any{"generated_at": time.Now(), "project_root": e.root, "config": e.cfg.Get(), "runtime": e.Status(), "state": snap, "memory": memItems, "task_summaries": summaries, "artifacts": e.getDebugArtifacts(), "code_index": func() any {
		if e.index == nil {
			return nil
		}
		return e.index.Snapshot()
	}()}
	return json.MarshalIndent(dump, "", "  ")
}

func (e *Engine) getDebugArtifacts() map[string]map[string]string {
	res := make(map[string]map[string]string)
	artPath := filepath.Join(e.root, ".loa", "artifacts")
	tasks, err := os.ReadDir(artPath)
	if err != nil {
		return res
	}
	for _, taskDir := range tasks {
		if !taskDir.IsDir() {
			continue
		}
		taskID := taskDir.Name()
		taskRes := make(map[string]string)
		files, err := os.ReadDir(filepath.Join(artPath, taskID))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			b, err := os.ReadFile(filepath.Join(artPath, taskID, f.Name()))
			if err == nil {
				taskRes[f.Name()] = string(b)
			}
		}
		res[taskID] = taskRes
	}
	return res
}

func compactStepResults(results []state.StepResult) []map[string]any {
	var compacted []map[string]any
	for _, r := range results {
		compacted = append(compacted, map[string]any{
			"step_id": r.StepID,
			"success": r.Success,
			"summary": r.Summary,
		})
	}
	return compacted
}

func compactVerificationEvidence(snap state.AgentState) []map[string]any {
	if snap.ActiveTask == nil {
		return nil
	}
	referenced := map[uint64]uint64{}
	for _, step := range snap.ActiveTask.StepResults {
		for _, toolID := range step.ToolCallIDs {
			referenced[toolID] = step.StepID
		}
	}
	stepModes := make(map[uint64]state.StepMode, len(snap.ActiveTask.Plan.Steps))
	for _, step := range snap.ActiveTask.Plan.Steps {
		stepModes[step.ID] = step.Mode
	}
	results := make(map[uint64]state.ToolResult, len(snap.ToolResults))
	for _, result := range snap.ToolResults {
		results[result.ToolCallID] = result
	}
	out := []map[string]any{}
	for _, call := range snap.ToolCalls {
		stepID, ok := referenced[call.ID]
		if !ok {
			continue
		}
		switch call.Kind {
		case state.ToolExecuteProcess, state.ToolExecuteShell, state.ToolGitStatus, state.ToolGitDiff:
			// Command and diff evidence is useful regardless of the step mode.
		case state.ToolReadFile, state.ToolReadRange:
			// Raw file content can be large. Preserve reads from explicit verification
			// steps so the final evaluator can check the artifact instead of relying
			// only on a lossy step summary.
			if stepModes[stepID] != state.StepVerify {
				continue
			}
		default:
			continue
		}
		result, ok := results[call.ID]
		if !ok {
			continue
		}
		output := result.Output
		if len(output) > 2000 {
			output = output[:2000] + "\n...[truncated for final verification]"
		}
		out = append(out, map[string]any{
			"step_id":       stepID,
			"tool_call_id":  call.ID,
			"kind":          call.Kind,
			"description":   call.Description,
			"success":       result.Success,
			"exit_code":     result.ExitCode,
			"error":         result.Error,
			"output":        output,
			"was_truncated": result.Truncated,
		})
	}
	return out
}

func (e *Engine) handleSearchTaskSteps(call state.ToolCall) state.ToolResult {
	res := state.ToolResult{ToolCallID: call.ID, CreatedAt: time.Now()}
	input, ok := call.Input.(map[string]any)
	if !ok {
		res.Success = false
		res.Error = "invalid input format"
		return res
	}
	query, _ := input["query"].(string)
	if query == "" {
		res.Success = false
		res.Error = "query is required"
		return res
	}

	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		res.Success = false
		res.Error = "no active task"
		return res
	}

	queryLower := strings.ToLower(query)
	var matches []map[string]any

	for _, r := range snap.ActiveTask.StepResults {
		textToSearch := strings.ToLower(r.Summary)
		for _, f := range r.FactsLearned {
			textToSearch += " " + strings.ToLower(f)
		}
		for _, d := range r.Decisions {
			textToSearch += " " + strings.ToLower(d)
		}
		if strings.Contains(textToSearch, queryLower) {
			matches = append(matches, map[string]any{
				"step_id": r.StepID,
				"success": r.Success,
				"summary": r.Summary,
				"created_at": r.CreatedAt,
			})
		}
	}

	b, _ := json.MarshalIndent(matches, "", "  ")
	res.Success = true
	res.Output = "[SUPERSEDED WARNING: Older steps may contain outdated information. Check timestamps and step IDs.]\n" + string(b)
	return res
}

func (e *Engine) handleReadTaskStep(call state.ToolCall) state.ToolResult {
	res := state.ToolResult{ToolCallID: call.ID, CreatedAt: time.Now()}
	input, ok := call.Input.(map[string]any)
	if !ok {
		res.Success = false
		res.Error = "invalid input format"
		return res
	}
	
	stepIDFloat, ok := input["step_id"].(float64)
	if !ok {
		res.Success = false
		res.Error = "step_id must be a number"
		return res
	}
	stepID := uint64(stepIDFloat)

	snap := e.store.Snapshot()
	if snap.ActiveTask == nil {
		res.Success = false
		res.Error = "no active task"
		return res
	}

	for _, r := range snap.ActiveTask.StepResults {
		if r.StepID == stepID {
			b, _ := json.MarshalIndent(r, "", "  ")
			res.Success = true
			res.Output = string(b)
			return res
		}
	}
	
	res.Success = false
	res.Error = "step not found"
	return res
}
func compactVerificationConcerns(snap state.AgentState) []map[string]any {
	if snap.ActiveTask == nil {
		return nil
	}
	// Keep the latest concern from each evaluator type for each step. This
	// preserves defect/verification warnings without flooding the final context
	// with every intermediate "verify next" note emitted during a long step.
	outReversed := []map[string]any{}
	seenSlot := map[string]bool{}
	seenNeed := map[string]bool{}
	for i := len(snap.ExecutionLog) - 1; i >= 0; i-- {
		entry := snap.ExecutionLog[i]
		if entry.Kind != state.LogPrimitive || entry.TaskID == nil || *entry.TaskID != snap.ActiveTask.ID || entry.StepID == nil {
			continue
		}
		if entry.Primitive != "EvaluateActionResult" && entry.Primitive != "EvaluateStepResult" {
			continue
		}
		slot := fmt.Sprintf("%d:%s", *entry.StepID, entry.Primitive)
		if seenSlot[slot] {
			continue
		}
		seenSlot[slot] = true
		var reflection primitives.ReflectionResult
		if err := json.Unmarshal([]byte(entry.RawOutput), &reflection); err != nil {
			continue
		}
		need := strings.TrimSpace(reflection.VerificationNeed)
		if need == "" || seenNeed[need] {
			continue
		}
		seenNeed[need] = true
		item := map[string]any{
			"step_id":           *entry.StepID,
			"primitive":         entry.Primitive,
			"status":            reflection.Status,
			"reason":            reflection.Reason,
			"verification_need": need,
		}
		if len(reflection.Assumptions) > 0 {
			item["assumptions"] = reflection.Assumptions
		}
		outReversed = append(outReversed, item)
	}
	out := make([]map[string]any, len(outReversed))
	for i := range outReversed {
		out[len(outReversed)-1-i] = outReversed[i]
	}
	return out
}
