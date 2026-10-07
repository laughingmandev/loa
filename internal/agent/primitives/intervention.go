package primitives

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type InterventionResult struct {
	Action      string   `json:"action"`
	Reason      string   `json:"reason"`
	Summary     string   `json:"summary"`
	Facts       []string `json:"facts,omitempty"`
	Decisions   []string `json:"decisions,omitempty"`
	Constraints []string `json:"constraints,omitempty"`
	OpenIssues  []string `json:"open_issues,omitempty"`
	Missing     string   `json:"missing,omitempty"`
}

type InterventionInput struct {
	Purpose  string
	StepJSON string
}

type InterventionPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewInterventionPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *InterventionPrimitive {
	return &InterventionPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var InterventionPrompt = `Evaluate user guidance that arrived while an existing task was running. Treat the user's message as new task information, not as a separate task. Decide the smallest safe routing change needed.
Allowed action values are exactly: context_only, reconsider_current_step, replan_remaining, ask_user.
- context_only: guidance is useful context but the current step/remaining plan can continue unchanged.
- reconsider_current_step: the current step should be reconsidered using the new guidance, but the overall remaining plan is still structurally valid.
- replan_remaining: the new guidance invalidates assumptions, verification strategy, scope, or remaining plan and remaining work should be replanned while preserving completed steps.
- ask_user: the guidance is materially ambiguous and must be clarified before continuing.
Extract compact facts/decisions/constraints/open issues that future steps need. Do not invent information. User guidance cannot grant modification authority to a task that was created read-only; may_modify is immutable for the task. If guidance requests code changes during a read-only task, keep the task read-only and explain that conflict in open_issues or ask_user.
Return {"action":"context_only","reason":"short","summary":"brief summary of the guidance","facts":[],"decisions":[],"constraints":[],"open_issues":[]}.`

func ValidateIntervention(v *InterventionResult) error {
	if v.Reason == "" {
		return errors.New("reason is required")
	}
	if strings.TrimSpace(v.Summary) == "" {
		return errors.New("summary is required")
	}
	switch v.Action {
	case "context_only", "reconsider_current_step", "replan_remaining", "ask_user":
	default:
		return fmt.Errorf("invalid intervention action %q; allowed values: context_only, reconsider_current_step, replan_remaining, ask_user", v.Action)
	}
	return nil
}

func (p *InterventionPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in InterventionInput) (InterventionResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: InterventionPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)
	if in.StepJSON != "" {
		user += "\n\nCURRENT STEP AT GUIDANCE BOUNDARY:\n" + in.StepJSON
	}

	var out InterventionResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, user, &out, func() error {
		return ValidateIntervention(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("EvaluateIntervention", false)
		} else {
			pc.RecordInference("EvaluateIntervention", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "EvaluateIntervention", "evaluate intervention", user, a.Raw)
	}

	return out, err
}
