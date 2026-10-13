package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var hypothesisPrompt = `Before mutating the project state, explicitly state the scientific hypothesis you are testing.
What are you trying to accomplish, why do you believe this specific modification is correct, and what is the exact observable expected result?
Return {"hypothesis":"your reasoning","expected_result":"observable outcome"}.`

type HypothesisResult struct {
	Hypothesis     string `json:"hypothesis"`
	ExpectedResult string `json:"expected_result"`
}

type FormulateHypothesisInput struct {
	Purpose string
	Step    *state.PlanStep
	Extras  []string
}

type FormulateHypothesisPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewFormulateHypothesisPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *FormulateHypothesisPrimitive {
	return &FormulateHypothesisPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *FormulateHypothesisPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in FormulateHypothesisInput) (HypothesisResult, error) {
	var h HypothesisResult
	req := PromptRequest{
		PrimitiveInstructions: hypothesisPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	attempts, err := p.llm.ChatJSON(
		ctx,
		p.cfg.ModelExecuting,
		p.baseSystem,
		func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	},
		&h,
		func() error { return validateHypothesis(&h) },
	)

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("FormulateHypothesis", false)
		} else {
			pc.RecordInference("FormulateHypothesis", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "FormulateHypothesis", "formulate hypothesis", in.Purpose, a.Raw)
	}

	return h, err
}

func validateHypothesis(v *HypothesisResult) error {
	if strings.TrimSpace(v.Hypothesis) == "" {
		return errors.New("hypothesis required")
	}
	if strings.TrimSpace(v.ExpectedResult) == "" {
		return errors.New("expected_result required")
	}
	return nil
}
