package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

var iterativeEvaluationModifyingPrompt = `You are the final auditor checking if the task is complete.
You are provided the task goal and the current state of your checklist. 
Your job is to update the checklist by verifying each unverified criterion. 
If a criterion is unverified, output action="tool_call" to fetch evidence (e.g. read_artifact, search_memory).
If you find concrete evidence (codebase diffs, test logs, structural proofs) that proves a criterion, update its status to "verified" and write a short summary of the evidence in the checklist so you can remember it. You do not need to keep the raw data.
If checking a criterion with dependencies, ensure you review the evidence of its dependencies.
If you definitively prove a criterion cannot be met, output action="failed" with the reason.
If ALL criteria are "verified", output action="complete".`

var iterativeEvaluationReadOnlyPrompt = `You are the final auditor checking if the investigatory/read-only task is complete.
You are provided the task goal and the current state of your checklist. 
Your job is to update the checklist by verifying each unverified criterion.
If a criterion is unverified, output action="tool_call" to fetch evidence (e.g. read_artifact, search_memory).
Unlike modifying tasks, read-only tasks do not require concrete codebase diffs or execution logs. Proof that data was gathered, summarized, or an artifact was drafted is sufficient evidence.
If you find this evidence, update the criterion's status to "verified" and write a short summary of the evidence in the checklist so you can remember it.
If ALL criteria are "verified", output action="complete".`

type IterativeEvaluationResult struct {
	Checklist []state.AcceptanceCheck `json:"checklist"`
	Action    string                  `json:"action"` // "tool_call", "complete", "failed"
	Tool      *tools.Request          `json:"tool,omitempty"`
	Reason    string                  `json:"reason,omitempty"`
}

type IterativeEvaluationInput struct {
	Purpose    string
	Checklist  []state.AcceptanceCheck
	IsReadOnly bool
}

type IterativeEvaluationPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewIterativeEvaluationPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *IterativeEvaluationPrimitive {
	return &IterativeEvaluationPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *IterativeEvaluationPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in IterativeEvaluationInput) (IterativeEvaluationResult, error) {
	prompt := iterativeEvaluationModifyingPrompt
	if in.IsReadOnly {
		prompt = iterativeEvaluationReadOnlyPrompt
	}

	checklistJSON := "[\n"
	for _, check := range in.Checklist {
		checklistJSON += fmt.Sprintf(`  {"criterion_id": %d, "status": "%s", "evidence": %q}`, check.CriterionID, check.Status, check.Evidence) + ",\n"
	}
	checklistJSON += "]"

	req := PromptRequest{
		PrimitiveInstructions: prompt,
		PrimitiveInputLabel:   "\nTASK PURPOSE:\n",
		PrimitiveInput:        in.Purpose + "\n\nCURRENT CHECKLIST:\n" + checklistJSON,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out IterativeEvaluationResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, func() error {
		return validateIterativeEvaluation(&out, in.Checklist)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("IterativeEvaluation", false)
		} else {
			pc.RecordInference("IterativeEvaluation", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "IterativeEvaluation", "iterative evaluation loop", user, a.Raw)
	}

	return out, err
}

func validateIterativeEvaluation(v *IterativeEvaluationResult, original []state.AcceptanceCheck) error {
	if v.Action != "tool_call" && v.Action != "complete" && v.Action != "failed" {
		return fmt.Errorf("action must be 'tool_call', 'complete', or 'failed'")
	}
	if v.Action == "tool_call" && v.Tool == nil {
		return fmt.Errorf("tool_call action requires a tool object")
	}
	if v.Action == "failed" && v.Reason == "" {
		return fmt.Errorf("failed action requires a reason")
	}

	if len(v.Checklist) != len(original) {
		return fmt.Errorf("checklist must contain exactly %d items", len(original))
	}

	validIDs := make(map[uint64]bool)
	for _, c := range original {
		validIDs[c.CriterionID] = true
	}

	for _, c := range v.Checklist {
		if !validIDs[c.CriterionID] {
			return fmt.Errorf("invalid criterion_id %d in checklist", c.CriterionID)
		}
		if c.Status != state.AcceptanceVerified && c.Status != state.AcceptancePartial && c.Status != state.AcceptanceUnverified {
			return fmt.Errorf("invalid status %s for criterion %d", c.Status, c.CriterionID)
		}
	}

	return nil
}
