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

type RecoveryDecision struct {
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Missing string `json:"missing,omitempty"`
}

type AssessTaskRecoveryInput struct {
	Purpose string
}

type AssessTaskRecoveryPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewAssessTaskRecoveryPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *AssessTaskRecoveryPrimitive {
	return &AssessTaskRecoveryPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var AssessTaskRecoveryPrompt = `A previous coding task stopped with a hard error and is still preserved as recoverable state. Decide how the latest task-like user message relates to that failed task.
Allowed action values are exactly: continue, retry, replan, ask_user, start_new.
- continue: existing remaining plan is still valid and can proceed from current state.
- retry: retry the failed step with the existing plan.
- replan: preserve completed work but create a new remaining plan because the old route may be stale or incomplete.
- ask_user: the user's intent about recovery versus a new task is genuinely unclear.
- start_new: the latest request is a different task; archive the failed task and begin a new one.
Do not choose start_new merely because the user rephrased or expanded the same task.
Return {"action":"replan","reason":"short","missing":"what must be clarified if ask_user"}.`

func ValidateRecovery(r *RecoveryDecision) error {
	if r.Reason == "" {
		return errors.New("reason is required")
	}
	switch r.Action {
	case "continue", "retry", "replan", "ask_user", "start_new":
	default:
		return fmt.Errorf("invalid recovery action %q; allowed values: continue, retry, replan, ask_user, start_new", r.Action)
	}
	if r.Action == "ask_user" && strings.TrimSpace(r.Missing) == "" {
		return errors.New("missing is required when recovery action is ask_user")
	}
	return nil
}

func (p *AssessTaskRecoveryPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in AssessTaskRecoveryInput) (RecoveryDecision, error) {
	req := PromptRequest{
		PrimitiveInstructions: AssessTaskRecoveryPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out RecoveryDecision
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateRecovery(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("AssessTaskRecovery", false)
		} else {
			pc.RecordInference("AssessTaskRecovery", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "AssessTaskRecovery", "assess task recovery", user, a.Raw)
	}

	return out, err
}
