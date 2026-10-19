package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/memory"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

type scriptedLLM struct {
	mu            sync.Mutex
	json          []string
	text          string
	inf           uint64
	emb           uint64
	embeddedTexts []string
}

func (f *scriptedLLM) Models(context.Context) ([]string, error) { return []string{"fake"}, nil }

func firstPlanStepID(prompt string) (string, error) {
	idx := strings.LastIndex(prompt, "\nPLAN DRAFT:\n")
	if idx < 0 {
		idx = strings.LastIndex(prompt, "\nPLAN:\n")
	}
	if idx < 0 {
		idx = strings.LastIndex(prompt, "PLAN:\n")
	}
	if idx < 0 {
		return "", errors.New("plan not found in scripted prompt")
	}
	tail := prompt[idx:]
	idIdx := strings.Index(tail, `"id":`)
	if idIdx < 0 {
		return "", errors.New("plan step id not found in scripted prompt")
	}
	tail = strings.TrimSpace(tail[idIdx+len(`"id":`):])
	end := strings.IndexAny(tail, ",}\n\r\t ")
	if end < 0 {
		end = len(tail)
	}
	if end == 0 {
		return "", errors.New("plan step id was empty in scripted prompt")
	}
	return tail[:end], nil
}
func (f *scriptedLLM) ChatJSON(_ context.Context, model string, _ string, buildPrompt func(repairCost int) (string, error), out any, validate func() error) ([]llm.Attempt, error) {
	prompt, _ := buildPrompt(0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.json) == 0 {
		return nil, errors.New("no scripted JSON response")
	}
	raw := f.json[0]
	f.json = f.json[1:]
	fmt.Printf("[TEST MOCK] CallJSON prompt=%q response=%s\n", prompt, raw)
	if strings.Contains(raw, "__FIRST_PLAN_STEP_ID__") {
		stepID, err := firstPlanStepID(prompt)
		if err != nil {
			return nil, err
		}
		raw = strings.ReplaceAll(raw, "__FIRST_PLAN_STEP_ID__", stepID)
	}
	f.inf++
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return []llm.Attempt{{PromptKind: "original", Prompt: prompt, Raw: raw, Error: err.Error()}}, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			fmt.Printf("[TEST MOCK] validation failed for %q: %v\n", raw, err)
			return []llm.Attempt{{PromptKind: "original", Prompt: prompt, Raw: raw, Error: err.Error()}}, err
		}
	}
	return []llm.Attempt{{PromptKind: "original", Prompt: prompt, Raw: raw}}, nil
}
func (f *scriptedLLM) ChatText(ctx context.Context, model string, sys string, prompt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inf++
	return f.text, nil
}
func (f *scriptedLLM) Embed(_ context.Context, text string) ([]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emb++
	f.embeddedTexts = append(f.embeddedTexts, text)
	return []float32{1, 0}, nil
}
func (f *scriptedLLM) Stats() state.RuntimeStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return state.RuntimeStats{Inferences: f.inf, Embeddings: f.emb}
}

func TestSimpleTaskLifecycle(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingModel = ""
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	session, err := st.CreateSession("Task test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSessionModes("", false)
	tm := tools.New(root, nil, cfg.Get, nil)
	fake := &scriptedLLM{text: "Implemented and verified.", json: []string{
		`{"intents":["task"],"answers_pending_question":false,"ambiguous":false,"reason":"explicit task"}`,
		`{"items":[]}`,
		`{"action":"ready_to_plan","reason":"task is self-contained"}`,
		`{"criteria":["The requested change is completed and verified."],"reason":"captures the requested outcome"}`,
		`{"mappings":[]}`,
		`{"target":"do task"}`,
		`{"version":1,"steps":[{"title":"Do task","goal":"complete requested change","reason":"needed","expected_result":"task completed","mode":"modify","depends_on":[],"expected_tools":[]}],"reason":"simple plan"}`,
		`{"tags":[{"step_id":1,"entities":["target1"]}]}`,
		`{"valid":true,"reason":"plan is sufficient","coverage":[{"criterion_id":1,"step_ids":[__FIRST_PLAN_STEP_ID__],"reason":"the only plan step covers the requested outcome"}],"issues":[]}`,
		`{"index_keys":["plan", "keywords"],"reason":"plan memory"}`,
		`{"executable":true,"reason":"one coherent objective"}`,
		`{"index_keys":["step", "keywords"],"reason":"step memory"}`,
		`{"action":"step_complete","reason":"work is complete","completion_summary":"completed task"}`,
		`{"status":"success","reason":"completion claim satisfies step","assumptions":[],"suggested_recovery":"","verification_need":""}`,
		`{"artifact_payload":""}`,
		`{"summary":"Task completed successfully.","facts_learned":[],"decisions":[]}`,
		`{"facts":[],"decisions":[],"constraints":[],"open_issues":[]}`,
		`{"index_keys":["task", "completed"],"reason":"keywords for step summary"}`,
		`{"action":"complete","reason":"all requested work is complete","checklist":[{"criterion_id":1,"status":"verified","evidence":["completed step result confirms the requested outcome"]}]}`,
		`{"action":"tool","reason":"writing final response","tool":{"kind":"artifact_write","input":{"name":"final_response.md","content":"Implemented and verified."}}}`,
		`{"action":"step_complete","reason":"done","completion_summary":"wrote final response"}`,
		`{"summary":"Completed the requested task and verified the result."}`,
		`{"index_keys":["completed", "verified"],"reason":"extracted keywords"}`,
		`{"index_keys":["final", "response"],"reason":"task memory"}`,
	}}
	memStore := memory.NewStore(root, 336.0, 0.03)
	eng := New(root, st, memStore, cfg, fake, tm, nil)
	if err := eng.SubmitMessage("do the thing"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for eng.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if eng.Status().Running {
		t.Fatal("engine did not finish planning")
	}
	if eng.Status().LastError != "" {
		t.Fatalf("unexpected engine error: %s", eng.Status().LastError)
	}
	snap := st.Snapshot()
	if snap.ActiveTask == nil || snap.ActiveTask.Status != state.TaskAwaitingApproval {
		t.Fatalf("expected task to be awaiting approval, got %v", snap.ActiveTask)
	}

	st.UpdateActiveTask(func(t *state.TaskState) {
		t.Status = state.TaskRunning
	})
	if err := eng.StartActiveTask(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for eng.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if eng.Status().Running {
		t.Fatal("engine did not finish execution")
	}
	if eng.Status().LastError != "" {
		t.Fatalf("unexpected engine error during execution: %s", eng.Status().LastError)
	}
	stats := eng.Status().Stats
	var brokenDown uint64
	for _, n := range stats.InferenceByPrimitive {
		brokenDown += n
	}
	if brokenDown != stats.Inferences {
		t.Fatalf("inference breakdown should cover total calls: total=%d breakdown=%d %#v", stats.Inferences, brokenDown, stats.InferenceByPrimitive)
	}
	snap = st.Snapshot()
	if snap.ActiveTask != nil {
		t.Fatalf("active task should be cleared, state=%s, stats=%#v, task=%+v", snap.ActiveTask.Status, stats, snap.ActiveTask)
	}
	if len(snap.CompletedTasks) != 1 || snap.CompletedTasks[0].Status != state.TaskCompleted {
		t.Fatalf("completed task mismatch: %+v", snap.CompletedTasks)
	}
	if len(eng.SessionMemStore().Summaries()) != 1 {
		t.Fatalf("expected task summary, got %d", len(eng.SessionMemStore().Summaries()))
	}
	if len(snap.Messages) < 2 || snap.Messages[len(snap.Messages)-1].Text != "Implemented and verified." {
		t.Fatalf("missing final response: %+v", snap.Messages)
	}
	if _, err := os.Stat(filepath.Join(state.SessionsDir(root), session.ID+".json")); err != nil {
		t.Fatalf("expected auto-saved session state: %v", err)
	}
	if st.PersistenceStatus().Dirty {
		t.Fatal("completed task autosave should include the final response and leave state clean")
	}
	reloaded, err := state.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	msgs := reloaded.Snapshot().Messages
	if len(msgs) == 0 || msgs[len(msgs)-1].Text != "Implemented and verified." {
		t.Fatalf("final response was not persisted by autosave: %+v", msgs)
	}
}

func TestApprovalDenialIsBlockingAndNonFatal(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.Permissions.Mode = config.PermissionSelected
	cfgv.Permissions.AskFor[state.ToolWriteFile] = true
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	fake := &scriptedLLM{}
	tm := tools.New(root, nil, cfg.Get, nil)
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tm, nil)
	req := tools.Request{Kind: state.ToolWriteFile, Description: "write test file", Input: json.RawMessage(`{"path":"x.txt","content":"hello"}`)}
	type result struct {
		r   state.ToolResult
		err error
	}
	done := make(chan result, 1)
	go func() { r, err := eng.executeTool(context.Background(), req, true); done <- result{r, err} }()
	var id uint64
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snap := st.Snapshot()
		if len(snap.ToolCalls) > 0 {
			id = snap.ToolCalls[0].ID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == 0 {
		t.Fatal("approval request did not appear")
	}
	select {
	case <-done:
		t.Fatal("tool completed before approval")
	default:
	}
	if err := eng.Approve(id, false, false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.r.Error != "permission_denied" {
			t.Fatalf("unexpected result: %+v", got.r)
		}
	case <-time.After(time.Second):
		t.Fatal("tool did not unblock after denial")
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied write created file: %v", err)
	}
}

func TestValidPlanRequiresAcceptanceCoverage(t *testing.T) {
	criteria := []state.AcceptanceCriterion{
		{ID: 1, Requirement: "first outcome"},
		{ID: 2, Requirement: "second outcome"},
	}
	plan := state.Plan{Steps: []state.PlanStep{
		{ID: 101, Title: "First", Status: state.StepPending},
		{ID: 102, Title: "Second", Status: state.StepPending},
	}}

	ev := primitives.PlanEvaluation{
		Valid:  true,
		Reason: "complete",
		Coverage: []primitives.CriterionCoverage{
			{CriterionID: 1, StepIDs: []uint64{101}, Reason: "step 101 covers criterion 1"},
		},
	}
	if err := primitives.ValidatePlanEvaluation(&ev, criteria, plan); err == nil || !strings.Contains(err.Error(), "missing coverage for acceptance criterion id 2") {
		t.Fatalf("expected missing criterion coverage rejection, got %v", err)
	}

	ev.Coverage = append(ev.Coverage, primitives.CriterionCoverage{CriterionID: 2, StepIDs: []uint64{999}, Reason: "bad step"})
	if err := primitives.ValidatePlanEvaluation(&ev, criteria, plan); err == nil || !strings.Contains(err.Error(), "unknown plan step id 999") {
		t.Fatalf("expected unknown step rejection, got %v", err)
	}

	ev.Coverage[1] = primitives.CriterionCoverage{CriterionID: 2, StepIDs: []uint64{102}, Reason: "step 102 covers criterion 2"}
	if err := primitives.ValidatePlanEvaluation(&ev, criteria, plan); err != nil {
		t.Fatalf("complete coverage should validate: %v", err)
	}

	ev.Valid = false
	ev.Coverage[1].StepIDs = nil
	ev.Coverage[1].Reason = "criterion 2 is not covered by the current plan"
	if err := primitives.ValidatePlanEvaluation(&ev, criteria, plan); err != nil {
		t.Fatalf("invalid plan may explicitly report an uncovered criterion: %v", err)
	}
}

func TestExecutionToolKindActionGetsSemanticRepairHint(t *testing.T) {
	d := primitives.ExecutionDecision{
		Action: "execute_shell",
		Reason: "run verification",
		Tool: &tools.Request{
			Kind:  state.ToolExecuteShell,
			Input: json.RawMessage(`{"command":"go test ./..."}`),
		},
	}
	err := primitives.ValidateExecution(&d)
	if err == nil {
		t.Fatal("expected validation error")
	}
	msg := err.Error()
	for _, want := range []string{`action="tool"`, `tool.kind="execute_shell"`, "preserve the existing tool.input"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("repair hint %q missing from %q", want, msg)
		}
	}
}

func TestRerankUsesStableCandidateIDs(t *testing.T) {
	r := RerankResult{SelectedStructuralIDs: []uint64{335, 83}, Reason: "relevant", NeedsUser: false}
	valid := map[uint64]bool{335: true, 83: true, 45: true}
	if err := validateRerank(&r, valid, 2); err != nil {
		t.Fatalf("stable IDs should validate: %v", err)
	}
	r.SelectedStructuralIDs = []uint64{0}
	if err := validateRerank(&r, valid, 2); err == nil {
		t.Fatal("array position should not validate as a candidate ID")
	}
}

func TestFormatToolResultPrintsNumericExitCode(t *testing.T) {
	code := 0
	res := state.ToolResult{Success: true, ExitCode: &code}
	req := tools.Request{Kind: state.ToolExecuteProcess, Description: "verify", Input: json.RawMessage(`{"binary":"go","args":["test","./..."]}`)}
	got := formatToolResult(req, res)
	if !strings.Contains(got, "EXIT: 0") {
		t.Fatalf("expected numeric exit code, got %q", got)
	}
	if strings.Contains(got, "0x") {
		t.Fatalf("exit code should not render pointer address: %q", got)
	}
}

func TestCompactVerificationEvidenceIncludesReadsFromVerifySteps(t *testing.T) {
	exit := 0
	snap := state.AgentState{
		ActiveTask: &state.TaskState{
			ID: 42,
			Plan: state.Plan{Steps: []state.PlanStep{
				{ID: 10, Mode: state.StepVerify},
				{ID: 11, Mode: state.StepInvestigate},
			}},
			StepResults: []state.StepResult{
				{StepID: 10, ToolCallIDs: []uint64{1}},
				{StepID: 11, ToolCallIDs: []uint64{2, 3, 4}},
			},
		},
		ToolCalls: []state.ToolCall{
			{ID: 1, Kind: state.ToolReadFile, Description: "verify README"},
			{ID: 2, Kind: state.ToolReadFile, Description: "investigate source"},
			{ID: 3, Kind: state.ToolGitDiff, Description: "inspect final diff"},
			{ID: 4, Kind: state.ToolExecuteProcess, Description: "run tests"},
		},
		ToolResults: []state.ToolResult{
			{ToolCallID: 1, Success: true, Output: "README contents"},
			{ToolCallID: 2, Success: true, Output: "source contents"},
			{ToolCallID: 3, Success: true, Output: "diff contents"},
			{ToolCallID: 4, Success: true, Output: "ok", ExitCode: &exit},
		},
	}

	evidence := compactVerificationEvidence(snap)
	got := map[uint64]bool{}
	for _, item := range evidence {
		id, ok := item["tool_call_id"].(uint64)
		if !ok {
			t.Fatalf("tool_call_id has unexpected type: %#v", item["tool_call_id"])
		}
		got[id] = true
	}
	for _, want := range []uint64{1, 3, 4} {
		if !got[want] {
			t.Fatalf("expected tool call %d in verification evidence: %#v", want, evidence)
		}
	}
	if got[2] {
		t.Fatalf("investigative read should not be copied into final verification evidence: %#v", evidence)
	}
}

func TestCompactVerificationConcernsPreservesCurrentTaskNeeds(t *testing.T) {
	taskID := uint64(42)
	otherTaskID := uint64(99)
	stepID := uint64(10)
	reflection := `{"status":"success","reason":"file written","assumptions":["README may still contain a typo"],"verification_need":"Verify README for the dbsi typo before completion."}`
	snap := state.AgentState{
		ActiveTask: &state.TaskState{ID: taskID},
		ExecutionLog: []state.ExecutionLogEntry{
			{Kind: state.LogPrimitive, TaskID: &taskID, StepID: &stepID, Primitive: "EvaluateActionResult", RawOutput: reflection},
			{Kind: state.LogPrimitive, TaskID: &taskID, StepID: &stepID, Primitive: "EvaluateStepResult", RawOutput: reflection},
			{Kind: state.LogLLM, TaskID: &taskID, StepID: &stepID, Primitive: "EvaluateActionResult", RawOutput: reflection},
			{Kind: state.LogPrimitive, TaskID: &otherTaskID, StepID: &stepID, Primitive: "EvaluateActionResult", RawOutput: `{"status":"success","reason":"other","verification_need":"other task concern"}`},
		},
	}

	concerns := compactVerificationConcerns(snap)
	if len(concerns) != 1 {
		t.Fatalf("expected one deduplicated current-task concern, got %#v", concerns)
	}
	if got := concerns[0]["verification_need"]; got != "Verify README for the dbsi typo before completion." {
		t.Fatalf("unexpected verification concern: %#v", got)
	}
	if got := concerns[0]["step_id"]; got != stepID {
		t.Fatalf("unexpected concern step id: %#v", got)
	}
}

func TestVerificationPromptsPreserveEvidenceAndLiterals(t *testing.T) {
	for _, want := range []string{"preserve them exactly", "[ON]/[OFF]", "omit it rather than mutate it"} {
		if !strings.Contains(primitives.TaskContextPrompt, want) {
			t.Fatalf("RefreshTaskContext prompt missing literal-preservation instruction %q", want)
		}
	}
	for _, want := range []string{"prior verification concerns", "still contains that defect", "contradictory concrete evidence"} {
		if !strings.Contains(finalCheckPrompt, want) {
			t.Fatalf("FinalTaskEvaluation prompt missing verification hardening %q", want)
		}
	}
}

func TestAllowRunAutoApprovesOnlyExactCommand(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.Permissions.Mode = config.PermissionSelected
	cfgv.Permissions.AskFor[state.ToolExecuteShell] = true
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	tm := tools.New(root, nil, cfg.Get, nil)
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, &scriptedLLM{}, tm, nil)

	type result struct {
		r   state.ToolResult
		err error
	}
	runAsync := func(req tools.Request) chan result {
		ch := make(chan result, 1)
		go func() {
			r, err := eng.executeTool(context.Background(), req, true)
			ch <- result{r: r, err: err}
		}()
		return ch
	}
	waitPending := func(after int) state.ToolCall {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			snap := st.Snapshot()
			if len(snap.ToolCalls) > after {
				call := snap.ToolCalls[len(snap.ToolCalls)-1]
				if call.RequiresApproval && call.Approved == nil {
					return call
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("approval request did not appear")
		return state.ToolCall{}
	}

	exact := tools.Request{Kind: state.ToolExecuteShell, Description: "list command", Input: json.RawMessage(`{"command":"printf exact","timeout_seconds":5}`)}
	firstDone := runAsync(exact)
	first := waitPending(0)
	if err := eng.Approve(first.ID, true, true); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-firstDone:
		if got.err != nil || !got.r.Success || got.r.Output != "exact" {
			t.Fatalf("unexpected first result: %+v err=%v", got.r, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("first command did not complete")
	}

	secondDone := make(chan result, 1)
	go func() {
		r, err := eng.executeTool(context.Background(), exact, true)
		secondDone <- result{r: r, err: err}
	}()
	select {
	case got := <-secondDone:
		if got.err != nil || !got.r.Success {
			t.Fatalf("exact repeated command should auto-approve: %+v err=%v", got.r, got.err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("exact repeated command unexpectedly waited for approval")
	}
	snap := st.Snapshot()
	second := snap.ToolCalls[len(snap.ToolCalls)-1]
	if second.Approved == nil || !*second.Approved || !second.RequiresApproval {
		t.Fatalf("auto-approved call should record approval policy and decision: %+v", second)
	}

	changed := tools.Request{Kind: state.ToolExecuteShell, Description: "changed command", Input: json.RawMessage(`{"command":"printf changed","timeout_seconds":5}`)}
	changedDone := runAsync(changed)
	third := waitPending(2)
	if err := eng.Approve(third.ID, false, false); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-changedDone:
		if got.err != nil || got.r.Error != "permission_denied" {
			t.Fatalf("changed command should require fresh approval: %+v err=%v", got.r, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("changed command did not unblock after denial")
	}
}

func TestInferenceBreakdownTracksPrimitivesAndRepairs(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewManager(config.Default())
	st := state.NewStore(root, state.NewAgentState())
	fake := &scriptedLLM{}
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)

	eng.recordInference("InterpretIntent", 2)
	eng.recordInference("JSONRepair", 3)
	eng.recordInference("GenerateFinalResponse", 1)
	stats := eng.Status().Stats
	if stats.InferenceByPrimitive["InterpretIntent"] != 2 || stats.InferenceByPrimitive["JSONRepair"] != 3 || stats.InferenceByPrimitive["GenerateFinalResponse"] != 1 {
		t.Fatalf("unexpected inference breakdown: %#v", stats.InferenceByPrimitive)
	}
}

func TestReadOnlyTaskUsesStructuredTaskLifecycle(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingModel = ""
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	if _, err := st.CreateSession("Read only task"); err != nil {
		t.Fatal(err)
	}
	st.SetSessionModes("", true)
	fake := &scriptedLLM{text: "Analysis complete.", json: []string{
		`{"intents":["task"],"answers_pending_question":false,"ambiguous":false,"reason":"structured read-only analysis"}`,
		`{"items":[]}`,
		`{"action":"ready_to_plan","reason":"task is self-contained"}`,
		`{"criteria":["The project architecture is analyzed from source code."],"reason":"captures the requested read-only outcome"}`,
		`{"mappings":[]}`,
		`{"target":"analyze architecture"}`,
		`{"version":1,"steps":[{"title":"Analyze file","goal":"determine answer","reason":"investigation needed","expected_result":"answer found","mode":"investigate","depends_on":[],"expected_tools":["read_file"]}],"reason":"simple analysis plan"}`,
		`{"tags":[{"step_id":1,"entities":["file.go"]}]}`,
		`{"valid":true,"reason":"plan covers the analysis","coverage":[{"criterion_id":1,"step_ids":[__FIRST_PLAN_STEP_ID__],"reason":"analysis step covers the criterion"}],"issues":[]}`,
		`{"index_keys":["plan", "keywords"],"reason":"plan memory"}`,
		`{"executable":true,"reason":"bounded investigation"}`,
		`{"index_keys":["step", "keywords"],"reason":"step memory"}`,
		`{"action":"step_complete","reason":"analysis is complete","completion_summary":"architecture analyzed from source"}`,
		`{"status":"success","reason":"analysis goal was satisfied","assumptions":[],"suggested_recovery":"","verification_need":""}`,
		`{"artifact_payload":""}`,
		`{"summary":"Analyzed the project architecture from source code.","facts_learned":["The engine coordinates agent execution."],"decisions":[]}`,
		`{"facts":["The engine coordinates agent execution."],"decisions":[],"constraints":[],"open_issues":[]}`,
		`{"index_keys":["analyzed", "architecture"],"reason":"keys for step summary"}`,
		`{"action":"complete","reason":"analysis is complete","checklist":[{"criterion_id":1,"status":"verified","evidence":["step output has the answer"]}]}`,
		`{"action":"tool","reason":"writing final response","tool":{"kind":"artifact_write","input":{"name":"final_response.md","content":"Analysis complete."}}}`,
		`{"action":"step_complete","reason":"done","completion_summary":"wrote final response"}`,
		`{"summary":"Completed a source-grounded architecture analysis."}`,
		`{"index_keys":["architecture", "analysis"],"reason":"extracted keys"}`,
		`{"index_keys":["final", "response"],"reason":"task memory"}`,
	}}
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)
	if err := eng.SubmitMessage("analyze this whole project from code only"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for eng.Status().Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if eng.Status().Running {
		t.Fatal("engine did not finish")
	}
	if eng.Status().LastError != "" {
		t.Fatalf("unexpected engine error: %s", eng.Status().LastError)
	}
	snap := st.Snapshot()
	if len(snap.CompletedTasks) != 1 {
		t.Fatalf("expected one completed task, got %d", len(snap.CompletedTasks))
	}
	if snap.CompletedTasks[0].MayModify {
		t.Fatal("read-only task unexpectedly gained modification authority")
	}
	if got := eng.Status().Stats.InferenceByPrimitive["Discuss"]; got != 0 {
		t.Fatalf("read-only task should use task machinery, not Discuss loop; got %d Discuss calls", got)
	}
}

func TestReadOnlyTaskRejectsModifyPlan(t *testing.T) {
	p := &primitives.PlanResult{Steps: []primitives.DraftStep{{
		Title:          "Change file",
		Goal:           "modify project",
		Reason:         "requested by model",
		ExpectedResult: "file changed",
		Mode:           state.StepModify,
	}}}
	if err := primitives.ValidatePlanForTask(p, false, 5); err == nil || !strings.Contains(err.Error(), "strictly read-only") {
		t.Fatalf("expected read-only plan rejection, got %v", err)
	}
	if err := primitives.ValidatePlanForTask(p, true, 5); err != nil {
		t.Fatalf("modify-authorized task should accept modify step: %v", err)
	}
	p.Steps[0].Mode = state.StepInvestigate
	p.Steps[0].ExpectedTools = []state.ToolKind{state.ToolWriteFile}
	if err := primitives.ValidatePlanForTask(p, false, 5); err == nil || !strings.Contains(err.Error(), "mutating tool") {
		t.Fatalf("expected read-only expected-tool rejection, got %v", err)
	}
}


func TestInterventionBecomesTaskGuidanceAndRequestsReplan(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingModel = ""
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	step := state.PlanStep{ID: 7, Title: "Run Laravel tests", Goal: "verify behavior", Mode: state.StepVerify, Status: state.StepRunning}
	st.SetActiveTask(&state.TaskState{
		ID:                 1,
		Goal:               "fix Laravel behavior",
		MayModify:          true,
		Status:             state.TaskRunning,
		Plan:               state.Plan{Version: 1, Steps: []state.PlanStep{step}},
		AcceptanceCriteria: []state.AcceptanceCriterion{{ID: 1, Requirement: "Requested behavior is corrected."}},
	})
	fake := &scriptedLLM{json: []string{
		`{"index_keys":["tests","laravel","containers"]}`,
		`{"action":"replan_remaining","reason":"local runtime tests are unavailable","summary":"Use non-runtime verification and stop retrying local tests.","facts":[],"decisions":[],"constraints":["Laravel tests cannot run locally because the runtime is in dedicated containers."],"open_issues":[],"missing":""}`,
		`{"items":[]}`,
		`{"criteria":["Requested behavior is corrected."],"reason":"environment guidance changes verification strategy, not the requested outcome"}`,
		`{"mappings":[]}`,
		`{"target":"fix laravel behavior"}`,
	}}
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)
	eng.mu.Lock()
	eng.running = true
	eng.mu.Unlock()
	guidance := "Tests cannot run on this machine; the Laravel runtime lives in dedicated containers. Do not retry them locally."
	if err := eng.SubmitMessage(guidance); err != nil {
		t.Fatal(err)
	}
	outcome, reason, err := eng.processInterventions(context.Background(), &step)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != "replan" || !strings.Contains(reason, "local runtime tests are unavailable") {
		t.Fatalf("expected replan from intervention, got outcome=%q reason=%q", outcome, reason)
	}
	snap := st.Snapshot()
	if len(snap.ActiveTask.Guidance) != 1 || snap.ActiveTask.Guidance[0].Text != guidance {
		t.Fatalf("verbatim task guidance was not preserved: %+v", snap.ActiveTask.Guidance)
	}
	if len(snap.ActiveTask.Context.Constraints) != 1 || !strings.Contains(snap.ActiveTask.Context.Constraints[0], "dedicated containers") {
		t.Fatalf("semantic intervention constraint missing: %+v", snap.ActiveTask.Context)
	}
	if len(snap.Messages) != 1 || !snap.Messages[0].Intervention {
		t.Fatalf("intervention message should be visible and marked as guidance: %+v", snap.Messages)
	}
}

func TestInterventionSupersedesPendingApproval(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.Permissions.Mode = config.PermissionAskAll
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	st.SetActiveTask(&state.TaskState{ID: 1, Goal: "active task", MayModify: true, Status: state.TaskRunning, Plan: state.Plan{Steps: []state.PlanStep{{ID: 2, Status: state.StepRunning, Mode: state.StepModify}}}})
	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, &scriptedLLM{}, tools.New(root, nil, cfg.Get, nil), nil)
	eng.mu.Lock()
	eng.running = true
	eng.mu.Unlock()
	type result struct {
		r   state.ToolResult
		err error
	}
	done := make(chan result, 1)
	req := tools.Request{Kind: state.ToolWriteFile, Description: "stale write", Input: json.RawMessage(`{"path":"stale.txt","content":"no"}`)}
	go func() {
		r, err := eng.executeTool(context.Background(), req, true)
		done <- result{r: r, err: err}
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		calls := st.Snapshot().ToolCalls
		if len(calls) > 0 && calls[0].Approved == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(st.Snapshot().ToolCalls) == 0 {
		t.Fatal("pending approval did not appear")
	}
	if err := eng.SubmitMessage("Do not perform that write; reconsider the current approach first."); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.r.Error != "intervention_superseded" {
			t.Fatalf("pending action should be superseded, got %+v err=%v", got.r, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending approval was not released by intervention")
	}
	if _, err := os.Stat(filepath.Join(root, "stale.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded write unexpectedly executed: %v", err)
	}
}

func TestValidateLinkReplanRules(t *testing.T) {
	completed := []state.PlanStep{
		{ID: 10, Title: "Investigate A", Status: state.StepCompleted},
		{ID: 11, Title: "Investigate B", Status: state.StepCompleted},
		{ID: 12, Title: "Investigate C", Status: state.StepCompleted},
		{ID: 13, Title: "Investigate D", Status: state.StepCompleted},
		{ID: 14, Title: "Investigate E", Status: state.StepCompleted},
	}
	newSteps := []state.PlanStep{
		{ID: 20, Title: "Synthesize root", DependsOn: nil, Status: state.StepPending},
		{ID: 21, Title: "Verify output", DependsOn: []uint64{20}, Status: state.StepPending},
	}

	// 1. Root new step left unlinked must fail validation (prevents disconnected graph).
	unlinkedRoot := primitives.LinkReplanResult{
		Mappings: []primitives.ReplanStepMapping{
			{StepID: 21, CompletedDependsOn: []uint64{10}},
		},
		Reason: "missed root",
	}
	if err := primitives.ValidateLinkReplan(&unlinkedRoot, completed, newSteps, 4); err == nil || !strings.Contains(err.Error(), "New root step 20") {
		t.Fatalf("expected unlinked root step rejection, got %v", err)
	}

	// 2. Putting a completed step ID in step_id must fail validation.
	wrongDirection := primitives.LinkReplanResult{
		Mappings: []primitives.ReplanStepMapping{
			{StepID: 10, CompletedDependsOn: []uint64{20}},
		},
		Reason: "reversed",
	}
	if err := primitives.ValidateLinkReplan(&wrongDirection, completed, newSteps, 4); err == nil || !strings.Contains(err.Error(), "already-completed step") {
		t.Fatalf("expected reversed step_id rejection, got %v", err)
	}

	// 3. Putting a new step ID in completed_depends_on must fail validation.
	newInCompleted := primitives.LinkReplanResult{
		Mappings: []primitives.ReplanStepMapping{
			{StepID: 20, CompletedDependsOn: []uint64{10}},
			{StepID: 21, CompletedDependsOn: []uint64{20}},
		},
		Reason: "new step in completed_depends_on",
	}
	if err := primitives.ValidateLinkReplan(&newInCompleted, completed, newSteps, 4); err == nil || !strings.Contains(err.Error(), "is a new pending step") {
		t.Fatalf("expected new step in completed_depends_on rejection, got %v", err)
	}

	// 4. Exceeding MaxPrerequisitesPerStep (internal + completed) must fail validation.
	exceedsLimit := primitives.LinkReplanResult{
		Mappings: []primitives.ReplanStepMapping{
			{StepID: 20, CompletedDependsOn: []uint64{10}},
			{StepID: 21, CompletedDependsOn: []uint64{10, 11, 12, 13}}, // 1 internal + 4 completed = 5 > 4
		},
		Reason: "too many deps on step 21",
	}
	if err := primitives.ValidateLinkReplan(&exceedsLimit, completed, newSteps, 4); err == nil || !strings.Contains(err.Error(), "exceeds the maximum allowed 4 prerequisites") {
		t.Fatalf("expected max prerequisites rejection, got %v", err)
	}

	// 5. Valid wiring across multiple completed branches succeeds and merges cleanly.
	validRes := primitives.LinkReplanResult{
		Mappings: []primitives.ReplanStepMapping{
			{StepID: 20, CompletedDependsOn: []uint64{11, 12, 13}},
			{StepID: 21, CompletedDependsOn: []uint64{10}},
		},
		Reason: "wired completed branches into new steps",
	}
	if err := primitives.ValidateLinkReplan(&validRes, completed, newSteps, 4); err != nil {
		t.Fatalf("expected valid wiring to pass, got %v", err)
	}
	wired := primitives.ApplyLinkReplanMappings(newSteps, validRes.Mappings)
	if len(wired[0].DependsOn) != 3 || wired[0].DependsOn[0] != 11 || wired[0].DependsOn[1] != 12 || wired[0].DependsOn[2] != 13 {
		t.Fatalf("unexpected dependencies on wired root step: %+v", wired[0].DependsOn)
	}
	if len(wired[1].DependsOn) != 2 || wired[1].DependsOn[0] != 10 || wired[1].DependsOn[1] != 20 {
		t.Fatalf("unexpected dependencies on wired child step: %+v", wired[1].DependsOn)
	}
}

func TestReplanWiresMultipleCompletedBranchesAndFeedsPrerequisites(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelPlanning = "fake"
	cfgv.MaxPrerequisitesPerStep = 4
	cfg := config.NewManager(cfgv)

	agState := state.NewAgentState()
	agState.NextID = 100 // deterministic new step IDs: 100, 101
	st := state.NewStore(root, agState)
	st.SetActiveTask(&state.TaskState{
		ID:        1,
		Goal:      "integrate completed investigations and apply fix",
		MayModify: true,
		Status:    state.TaskRunning,
		AcceptanceCriteria: []state.AcceptanceCriterion{
			{ID: 1, Requirement: "Fix is implemented and verified using findings from branches A and B."},
		},
		Plan: state.Plan{
			Version: 2,
			Steps: []state.PlanStep{
				{ID: 10, Title: "Base investigation", Goal: "inspect root", Mode: state.StepInvestigate, Status: state.StepCompleted},
				{ID: 11, Title: "Branch A analysis", Goal: "analyze module A", Mode: state.StepInvestigate, DependsOn: []uint64{10}, Status: state.StepCompleted},
				{ID: 12, Title: "Branch B analysis", Goal: "analyze module B", Mode: state.StepInvestigate, DependsOn: []uint64{10}, Status: state.StepCompleted},
				{ID: 50, Title: "Stale pending step", Goal: "old broken step", Mode: state.StepModify, DependsOn: []uint64{11}, Status: state.StepPending},
			},
		},
		StepResults: []state.StepResult{
			{StepID: 10, Success: true, Summary: "Root structure identified."},
			{StepID: 11, Success: true, Summary: "Branch A requires updating handler A."},
			{StepID: 12, Success: true, Summary: "Branch B requires updating config B."},
		},
	})

	fake := &scriptedLLM{json: []string{
		// 1. PlanPrimitive (Operation: "Replan")
		`{"steps":[
			{"title":"Implement integrated fix","goal":"apply changes for A and B","reason":"combine findings","expected_result":"code updated","mode":"modify","depends_on":[],"expected_tools":["write_file"]},
			{"title":"Verify integrated fix","goal":"run verification","reason":"confirm fix","expected_result":"tests pass","mode":"verify","depends_on":[1],"expected_tools":["execute_process"]}
		],"reason":"replanned remaining work"}`,
		// 2. TagTargetEntities
		`{"tags":[{"step_id":1,"entities":["handler.go"]},{"step_id":2,"entities":["handler_test.go"]}]}`,
		// 3. LinkReplanPrimitive (wires new step 103 -> completed [11, 12], and new step 104 -> completed [10])
		`{"mappings":[
			{"step_id":103,"completed_depends_on":[11,12]},
			{"step_id":104,"completed_depends_on":[10]}
		],"reason":"link root modify step to completed leaf branches 11 and 12, and verify step to base 10"}`,
		// 4. EvaluatePlanPrimitive
		`{"valid":true,"reason":"all criteria covered and graph connected","coverage":[{"criterion_id":1,"step_ids":[103,104],"reason":"steps 103 and 104 implement and verify the fix"}],"issues":[]}`,
	}}

	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)
	if err := eng.replan(context.Background(), "LinkSteps escalated to replan"); err != nil {
		t.Fatalf("replan failed: %v", err)
	}

	snap := st.Snapshot()
	steps := snap.ActiveTask.Plan.Steps
	if len(steps) != 5 {
		t.Fatalf("expected 3 completed + 2 new steps = 5 steps, got %d: %+v", len(steps), steps)
	}
	step103 := steps[3]
	step104 := steps[4]
	if step103.ID != 103 || len(step103.DependsOn) != 2 || step103.DependsOn[0] != 11 || step103.DependsOn[1] != 12 {
		t.Fatalf("expected step 103 to depend on completed steps [11, 12], got %+v", step103)
	}
	if step104.ID != 104 || len(step104.DependsOn) != 2 || step104.DependsOn[0] != 10 || step104.DependsOn[1] != 103 {
		t.Fatalf("expected step 104 to depend on [10, 103], got %+v", step104)
	}

	// Verify that getPrerequisites now feeds the completed step results into step 103!
	prereqs := getPrerequisites(&step103, snap.ActiveTask.StepResults)
	if len(prereqs) != 2 || prereqs[0].StepID != 11 || prereqs[1].StepID != 12 {
		t.Fatalf("expected prerequisites for step 103 to include completed steps 11 and 12, got %+v", prereqs)
	}
}

func TestLinkStepsIgnoresLegacyCompletedStepExceedingMaxPrerequisites(t *testing.T) {
	cfg := config.Default()
	cfg.ModelPlanning = "fake"
	cfg.MaxPrerequisitesPerStep = 4

	fake := &scriptedLLM{json: []string{
		`{"mappings":[{"receiver_id":30,"depends_on":[21,22]}],"reason":"link receiver 30 to decomposed steps"}`,
	}}

	prim := primitives.NewLinkStepsPrimitive(cfg, fake, "system")
	pc := &mockPrimitiveContext{}
	res, err := prim.Execute(context.Background(), pc, primitives.LinkStepsInput{
		OriginalStepID:    20,
		OriginalReceivers: []uint64{30},
		DecomposedSteps: []state.PlanStep{
			{ID: 21, Title: "Substep 1", DependsOn: []uint64{10}, Status: state.StepPending},
			{ID: 22, Title: "Substep 2", DependsOn: []uint64{21}, Status: state.StepPending},
		},
		GlobalPlan: state.Plan{
			Version: 1,
			Steps: []state.PlanStep{
				// Legacy completed step from before max-prerequisites rule with 5 dependencies:
				{ID: 10, Title: "Legacy completed step", DependsOn: []uint64{1, 2, 3, 4, 5}, Status: state.StepCompleted},
				{ID: 20, Title: "Step being decomposed", DependsOn: []uint64{10}, Status: state.StepRunning},
				{ID: 30, Title: "Pending receiver", DependsOn: []uint64{20}, Status: state.StepPending},
			},
		},
	})
	if err != nil {
		t.Fatalf("expected legacy completed step with >4 deps to be ignored by LinkSteps, got error: %v", err)
	}
	if len(res.Mappings) != 1 || res.Mappings[0].ReceiverID != 30 {
		t.Fatalf("unexpected LinkSteps result: %+v", res)
	}
}

func TestIterativeEvaluationCarriesExtrasAndFinishTaskWiresLeafDeps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "check.txt"), []byte("AUDITOR_SECRET_VALUE"), 0644); err != nil {
		t.Fatal(err)
	}
	cfgv := config.Default()
	cfgv.ModelPlanning = "fake"
	cfgv.MaxPrerequisitesPerStep = 4
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	st.SetActiveTask(&state.TaskState{
		ID:        1,
		Goal:      "verify check.txt and synthesize",
		MayModify: true,
		Status:    state.TaskRunning,
		AcceptanceCriteria: []state.AcceptanceCriterion{
			{ID: 1, Requirement: "check.txt contains AUDITOR_SECRET_VALUE"},
		},
		Plan: state.Plan{
			Version: 1,
			Steps: []state.PlanStep{
				{ID: 10, Title: "Root step", Status: state.StepCompleted},
				{ID: 11, Title: "Leaf A", DependsOn: []uint64{10}, Status: state.StepCompleted},
				{ID: 12, Title: "Leaf B", DependsOn: []uint64{10}, Status: state.StepCompleted},
			},
		},
		StepResults: []state.StepResult{
			{StepID: 10, Success: true, Summary: "Root done"},
			{StepID: 11, Success: true, Summary: "Leaf A done"},
			{StepID: 12, Success: true, Summary: "Leaf B done"},
		},
	})

	fake := &scriptedLLM{json: []string{
		`{"action":"tool_call","reason":"read check.txt","tool":{"kind":"read_file","input":{"path":"check.txt"}},"checklist":[{"criterion_id":1,"status":"unverified"}]}`,
		`{"action":"complete","reason":"verified check.txt","checklist":[{"criterion_id":1,"status":"verified","evidence":["AUDITOR_SECRET_VALUE found"]}]}`,
	}}

	eng := New(root, st, memory.NewStore(root, 336.0, 0.03), cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)
	check, err := eng.runIterativeEvaluation(context.Background(), false)
	if err != nil {
		t.Fatalf("runIterativeEvaluation failed: %v", err)
	}
	if !check.Complete {
		t.Fatalf("expected check.Complete=true, got %+v", check)
	}
	// Verify the second IterativeEvaluation prompt included the tool output from iteration 1 via Extras!
	snap := st.Snapshot()
	var evalLogs []state.ExecutionLogEntry
	for _, entry := range snap.ExecutionLog {
		if entry.Kind == state.LogLLM && entry.Primitive == "IterativeEvaluation" {
			evalLogs = append(evalLogs, entry)
		}
	}
	if len(evalLogs) != 2 {
		t.Fatalf("expected 2 IterativeEvaluation LLM calls, got %d", len(evalLogs))
	}
	if !strings.Contains(evalLogs[1].Input, "AUDITOR_SECRET_VALUE") {
		t.Fatalf("expected second IterativeEvaluation prompt to contain tool output via Extras, got:\n%s", evalLogs[1].Input)
	}

	// Verify finishTask wires Final Synthesis DependsOn to unconsumed leaf steps [11, 12] (not consumed root 10).
	err = eng.finishTask(context.Background(), check)
	if err == nil || err.Error() != "continue_synthesis" {
		t.Fatalf("expected continue_synthesis error on first finishTask call, got %v", err)
	}
	snap = st.Snapshot()
	steps := snap.ActiveTask.Plan.Steps
	synth := steps[len(steps)-1]
	if synth.Mode != state.StepSynthesize {
		t.Fatalf("expected last step to be StepSynthesize, got %+v", synth)
	}
	if len(synth.DependsOn) != 2 || synth.DependsOn[0] != 11 || synth.DependsOn[1] != 12 {
		t.Fatalf("expected Final Synthesis DependsOn=[11, 12], got %+v", synth.DependsOn)
	}
}

type mockPrimitiveContext struct{}

func (m *mockPrimitiveContext) BuildPrompt(req primitives.PromptRequest) (string, error) {
	return req.PrimitiveInput, nil
}
func (m *mockPrimitiveContext) Log(kind state.LogKind, primitive, summary, input, raw string) {}
func (m *mockPrimitiveContext) RecordInference(primitive string, isRepair bool)               {}

func TestEmbeddingOnlyUsesExtractedKeywords(t *testing.T) {
	root := t.TempDir()
	cfgv := config.Default()
	cfgv.ModelConversation = "fake"
	cfgv.EmbeddingEngine = "openapi"
	cfgv.EmbeddingModel = "fake-embed"
	cfg := config.NewManager(cfgv)
	st := state.NewStore(root, state.NewAgentState())
	_, err := st.CreateSession("Embed keywords test")
	if err != nil {
		t.Fatal(err)
	}
	memStore := memory.NewStore(root, 336.0, 0.03)
	fake := &scriptedLLM{
		json: []string{
			// 1. addMemory with empty index_keys -> should NOT call Embed
			`{"index_keys":[]}`,
			// 2. addMemory with extracted keywords -> should call Embed with "price stock parser"
			`{"index_keys":["price","stock","parser"]}`,
			// 3. runTask prefetchDiscussionMemory -> ExtractKeywords on raw goal
			`{"index_keys":["price","stock"]}`,
			// 4. PlanResearch ready_to_plan
			`{"action":"ready_to_plan","reason":"ready"}`,
			// 5. AcceptanceCriteria
			`{"criteria":["Check price stock parser."],"reason":"done"}`,
			`{"mappings":[]}`,
			// 6. CreateInitialPlan
			`{"target":"price stock parser"}`,
			`{"version":1,"steps":[{"title":"Inspect parser","goal":"inspect parser","reason":"check","expected_result":"inspected","mode":"modify","depends_on":[],"expected_tools":[]}],"reason":"plan"}`,
			`{"tags":[{"step_id":1,"entities":["parser"]}]}`,
			`{"valid":true,"reason":"ok","coverage":[{"criterion_id":1,"step_ids":[__FIRST_PLAN_STEP_ID__],"reason":"covered"}],"issues":[]}`,
		},
	}
	eng := New(root, st, memStore, cfg, fake, tools.New(root, nil, cfg.Get, nil), nil)

	// 1. When ExtractKeywords returns empty index_keys, addMemory must NOT embed the raw text.
	eng.addMemory(context.Background(), state.MemoryStructural, "RAW_UNFILTERED_TEXT_SHOULD_NEVER_BE_EMBEDDED", nil, nil, state.MessageUser)
	if len(fake.embeddedTexts) != 0 {
		t.Fatalf("expected 0 Embed calls when index_keys is empty, got %v", fake.embeddedTexts)
	}

	// 2. When ExtractKeywords returns index_keys, addMemory embeds only the joined keywords.
	eng.addMemory(context.Background(), state.MemoryStructural, "Full narrative frame about price stock parser", nil, nil, state.MessageUser)
	if len(fake.embeddedTexts) != 1 || fake.embeddedTexts[0] != "price stock parser" {
		t.Fatalf("expected Embed to receive only joined keywords 'price stock parser', got %v", fake.embeddedTexts)
	}

	// 3. When runTask starts with a huge raw user goal, it extracts keywords first and embeds only those keywords.
	hugeRawGoal := "HUGE_RAW_USER_GOAL_TEXT_THAT_MUST_NOT_GO_TO_EMBEDDER " + strings.Repeat("word ", 500)
	if err := eng.runTask(context.Background(), hugeRawGoal, true, "Test Task", "Planned"); err != nil {
		t.Fatalf("runTask failed: %v", err)
	}
	if len(fake.embeddedTexts) != 2 || fake.embeddedTexts[1] != "price stock" {
		t.Fatalf("expected runTask to embed only extracted keywords 'price stock', got %v", fake.embeddedTexts)
	}
}

