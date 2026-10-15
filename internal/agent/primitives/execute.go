package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

var executePrompt = `Work on exactly the current plan step. You may request one action at a time: a tool call, semantic memory retrieval, the past-task-summary list, reading the chat history, clarification from the user, or declare the step complete. Prefer compact symbol/range reads over huge files when practical, but use whole files when appropriate. Do not modify files unless modification is allowed. TASK MODIFICATION AUTHORIZATION in active task state is a hard boundary: may_modify=false means strictly read-only. Furthermore, if the CURRENT STEP mode is "investigate" or "verify", modifying tools are STRICTLY FORBIDDEN even if the task overall allows modification. If you discover a bug during a verify step, do NOT attempt to fix it with a tool; instead, return step_complete with a failed outcome so a repair step can be scheduled.
If encountering an unknown class/file, you MUST return retrieve_memory.
Do not use raw text search or path guessing to understand architecture. You MUST follow the pipeline: Retrieve Semantic Memory -> Identify Targets -> Use find_symbol
For retrieve_memory, you may optionally include "allowed_kinds" (e.g. ["task_summary", "fact", "structural"]) to strictly filter memory results to specific kinds.
The chat history with the user is NOT included in your context. If the task or user guidance refers to earlier discussion (e.g. "as you suggested", "like we discussed", "the plan from before"), return read_history to read it. offset 0 is the newest message, higher offsets go further back; count is optional and capped by the system. State in reason exactly what you are looking for, because only the information relevant to that reason is returned.
Available tool schemas are provided below.
Return exactly one of:
{"action":"tool","reason":"Short explanation of why you are taking this action and where you are in the overall step (acts as your memory trail)","tool":{"kind":"read_file","input":{"path":"relative/path"}}}
{"action":"retrieve_memory","reason":"short","query":"semantic search phrase","allowed_kinds":["task_summary","fact"]}
{"action":"task_summaries","reason":"short"}
{"action":"read_history","reason":"what you are looking for in the chat history","offset":0,"count":10}
{"action":"ask_user","reason":"short","ask_user_question":"what cannot be determined locally"}
{"action":"step_complete","reason":"short","completion_summary":"what was accomplished"}`

type ExecutionDecision struct {
	Action            string         `json:"action"`
	Reason            string         `json:"reason"`
	EvaluateAfter     *bool          `json:"evaluate_after,omitempty"`
	Query             string         `json:"query,omitempty"`
	AllowedKinds      []string       `json:"allowed_kinds,omitempty"`
	Offset            int            `json:"offset,omitempty"`
	Count             int            `json:"count,omitempty"`
	Tool              *tools.Request `json:"tool,omitempty"`
	AskUserQuestion   string         `json:"ask_user_question,omitempty"`
	CompletionSummary string         `json:"completion_summary,omitempty"`
}

type ExecuteStepInput struct {
	Step             state.PlanStep
	PinnedEvaluation string
	Extras           []string
	BlindTrail       []string
	ReadOnly         bool
	IsFastLane       bool
}

type ExecuteStepPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewExecuteStepPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ExecuteStepPrimitive {
	return &ExecuteStepPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *ExecuteStepPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ExecuteStepInput, evalMode string) (ExecutionDecision, error) {
	stepJSONBytes, _ := json.Marshal(in.Step)
	purpose := fmt.Sprintf("CURRENT STEP:\n%s\n\nAVAILABLE TOOLS:\n%s", string(stepJSONBytes), tools.ToolSchema(in.ReadOnly))

	prompt := executePrompt
	if evalMode == "Dynamic" {
		prompt += `

"evaluate_after": true/false - Set to true immediately after you complete a single cohesive task (e.g., fixing one specific issue, refactoring one component, or running a test suite). If the evaluation lists multiple different problems, do NOT try to fix all of them blindly. Fix ONE problem, then set evaluate_after=true to verify your fix. Set to false ONLY when you need to chain a few fast actions together to solve that single problem (like editing two related files simultaneously).`
	}
	if in.IsFastLane {
		prompt += `

FAST LANE RULE: You are operating in Fast Lane mode. There will be NO final synthesis step to clean up your work. If your task requires outputting massive amounts of data, you MUST use artifact_write/append/patch to use an artifact as a scratchpad. If you create multiple scratchpad artifacts, you are solely responsible for consolidating them into a single final artifact BEFORE you call step_complete. Your step_complete claim should simply act as a pointer (e.g., 'I extracted the data and saved it to final_report.md'). Do NOT attempt to cram massive data into your completion summary.`
	}

	req := PromptRequest{
		PrimitiveInstructions: prompt,
		PrimitiveInput:        purpose,
		Step:                  &in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        false,
		PinnedEvaluation:      in.PinnedEvaluation,
		BlindTrail:            in.BlindTrail,
	}
	var user string
	_ = user

	var out ExecutionDecision
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateExecution(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ExecuteStep", false)
		} else {
			pc.RecordInference("ExecuteStep", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ExecuteStep", "execute step", a.Prompt, a.Raw)
	}

	return out, err
}

func ValidateExecution(d *ExecutionDecision) error {
	// Auto-repair for common hallucination where Action is set to the tool kind
	if d.Action != "tool" && tools.ValidKind(state.ToolKind(d.Action)) {
		if d.Tool != nil {
			// The LLM provided the nested tool struct correctly, just mislabeled the action
			if d.Tool.Kind == "" {
				d.Tool.Kind = state.ToolKind(d.Action)
			}
			d.Action = "tool"
		}
	}

	if err := NeedReason(d.Reason); err != nil {
		return err
	}
	switch d.Action {
	case "retrieve_memory":
		if d.Query == "" {
			return fmt.Errorf("query required")
		}
	case "task_summaries":
	case "read_history":
		if d.Offset < 0 {
			return fmt.Errorf("offset must be >= 0 (0 is the newest message)")
		}
	case "tool":
		FillToolDescription(d.Reason, d.Tool)
		if err := ValidateToolRequest(d.Tool); err != nil {
			return err
		}
	case "ask_user":
		if d.AskUserQuestion == "" {
			return fmt.Errorf("ask_user_question required")
		}
	case "step_complete":
		if d.CompletionSummary == "" {
			return fmt.Errorf("completion_summary required")
		}
	default:
		// autorepair should cover this now ### but we keep it temporary
		if err := ToolActionRepairHint(d.Action); err != nil {
			return err
		}
		return fmt.Errorf("invalid execution action %q; allowed values: retrieve_memory, task_summaries, read_history, tool, ask_user, step_complete", d.Action)
	}
	return nil
}
