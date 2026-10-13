package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type CritiqueResult struct {
	Critique        string `json:"critique"`
	FalseAssumption string `json:"false_assumption"`
	NewApproach     string `json:"new_approach"`
}

type CritiqueApproachInput struct {
	Purpose  string
	Step     *state.PlanStep
	Extras   []string
}

type CritiqueApproachPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewCritiqueApproachPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *CritiqueApproachPrimitive {
	return &CritiqueApproachPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var CritiquePrompt = `You have repeatedly failed to make progress using this approach. The current route is logically flawed or based on incorrect assumptions.
Critique your previous actions. Identify the core false assumption. Formulate a new, completely different approach.
Return {"critique":"what went wrong","false_assumption":"the specific error in reasoning","new_approach":"what to do differently"}.`

func ValidateCritique(v *CritiqueResult) error {
	if strings.TrimSpace(v.Critique) == "" {
		return errors.New("critique required")
	}
	if strings.TrimSpace(v.FalseAssumption) == "" {
		return errors.New("false_assumption required")
	}
	if strings.TrimSpace(v.NewApproach) == "" {
		return errors.New("new_approach required")
	}
	return nil
}

func (p *CritiqueApproachPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in CritiqueApproachInput) (CritiqueResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: CritiquePrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out CritiqueResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateCritique(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("CritiqueApproach", false)
		} else {
			pc.RecordInference("CritiqueApproach", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "CritiqueApproach", "critique approach", user, a.Raw)
	}

	return out, err
}
