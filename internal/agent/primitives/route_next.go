package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type RouteResult struct {
	Action             string `json:"action"`
	Reason             string `json:"reason"`
	MissingInformation string `json:"missing_information,omitempty"`
}

type RouteNextInput struct {
	Purpose string
	StepJSON string
	Extras []string
}

type RouteNextPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewRouteNextPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *RouteNextPrimitive {
	return &RouteNextPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var RouteNextPrompt = `Choose a recovery route from the evaluator diagnosis.
Allowed action values are exactly: retry, investigate, repair, replan, rollback, ask_user.
Return {"action":"retry","reason":"short","missing_information":"..."}.`

func ValidateRoute(r *RouteResult) error {
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	switch r.Action {
	case "retry", "investigate", "repair", "replan", "rollback", "ask_user":
	default:
		return fmt.Errorf("invalid route action %q; allowed values: retry, investigate, repair, replan, rollback, ask_user", r.Action)
	}
	return nil
}

func (p *RouteNextPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in RouteNextInput) (RouteResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: RouteNextPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req) + "\n\nCURRENT STEP:\n" + in.StepJSON

	var out RouteResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, func() error {
		return ValidateRoute(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("RouteNext", false)
		} else {
			pc.RecordInference("RouteNext", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "RouteNext", "route next", user, a.Raw)
	}

	return out, err
}
