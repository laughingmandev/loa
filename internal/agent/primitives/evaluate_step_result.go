package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var reflectPrompt = `Independently evaluate the action/result against the intended step. Identify unverified assumptions.
Allowed status values are exactly: success, partial, failed, needs_more_information, plan_invalid, verification_required.
Allowed suggested_recovery values are exactly: retry, investigate, repair, replan, ask_user, or the empty string.
A write is not automatically success; request verification when appropriate. Do not call a behavior verified merely because a mock test passed if the intended requirement depends on real external behavior.
Return {"status":"success","reason":"short","assumptions":["..."],"suggested_recovery":"","verification_need":"..."}.`

type ReflectionResult struct {
	Status            string   `json:"status"`
	Reason            string   `json:"reason"`
	Assumptions       []string `json:"assumptions,omitempty"`
	SuggestedRecovery string   `json:"suggested_recovery,omitempty"`
	VerificationNeed  string   `json:"verification_need,omitempty"`
}

type EvaluateStepResultInput struct {
	Step   state.PlanStep
	Claim  string
	Extras []string
}

type EvaluateStepResultPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewEvaluateStepResultPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *EvaluateStepResultPrimitive {
	return &EvaluateStepResultPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *EvaluateStepResultPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in EvaluateStepResultInput) (ReflectionResult, error) {
	stepJSONBytes, _ := json.Marshal(in.Step)
	purpose := fmt.Sprintf("INTENDED STEP:\n%s\n\nEXECUTOR COMPLETION CLAIM:\n%s\n\nEvaluate whether the step is actually complete.", string(stepJSONBytes), in.Claim)

	req := PromptRequest{
		PrimitiveInstructions: reflectPrompt,
		PrimitiveInput:        purpose,
		Step:                  &in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out ReflectionResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, func() error {
		return ValidateReflection(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("EvaluateStepResult", false)
		} else {
			pc.RecordInference("EvaluateStepResult", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "EvaluateStepResult", "evaluate step result", user, a.Raw)
	}

	return out, err
}

func ValidateReflection(r *ReflectionResult) error {
	if r.Reason == "" {
		return fmt.Errorf("reason required")
	}
	switch r.Status {
	case "success", "partial", "failed", "needs_more_information", "plan_invalid", "verification_required":
	default:
		return fmt.Errorf("invalid reflection status %q; allowed values: success, partial, failed, needs_more_information, plan_invalid, verification_required", r.Status)
	}
	if r.SuggestedRecovery != "" {
		switch r.SuggestedRecovery {
		case "retry", "investigate", "repair", "replan", "ask_user":
		default:
			return fmt.Errorf("invalid suggested_recovery %q; allowed values: retry, investigate, repair, replan, ask_user, or empty string", r.SuggestedRecovery)
		}
	}
	return nil
}
