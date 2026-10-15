package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var assessBlindPrompt = `You have executed several modifying actions without formally evaluating the results.
Your task is to determine whether you should stop and formally evaluate your recent changes, or if you are confident enough to continue chaining actions blindly.

If you are just doing a repetitive task (like renaming a variable across many files) or are in the middle of a tightly coupled set of changes, choose "continue".
If you have completed a logical block of work, are unsure if the previous changes worked, or want a formal check, choose "evaluate".

Return exactly one of the following JSON structures:
{"decision": "evaluate", "reason": "short explanation"}
{"decision": "continue", "reason": "short explanation"}
`

type AssessBlindDecision struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

type AssessBlindInput struct {
	Step   *state.PlanStep
	Extras []string
}

type AssessBlindExecutionPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewAssessBlindExecutionPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *AssessBlindExecutionPrimitive {
	return &AssessBlindExecutionPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *AssessBlindExecutionPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in AssessBlindInput) (AssessBlindDecision, error) {
	req := PromptRequest{
		PrimitiveInstructions: assessBlindPrompt,
		Step:                  in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out AssessBlindDecision
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		if out.Decision != "evaluate" && out.Decision != "continue" {
			return fmt.Errorf("decision must be 'evaluate' or 'continue'")
		}
		if out.Reason == "" {
			return fmt.Errorf("reason is required")
		}
		return nil
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("AssessBlindExecution", false)
		} else {
			pc.RecordInference("AssessBlindExecution", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "AssessBlindExecution", "soft nudge decision", a.Prompt, a.Raw)
	}

	return out, err
}
