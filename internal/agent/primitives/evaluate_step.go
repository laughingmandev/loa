package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var stepEvalPrompt = `Judge whether the current plan step is realistically executable as one coherent execution cycle and can be independently evaluated afterward. Broad investigation steps spanning multiple independent components or flows MUST be rejected for decomposition. Return {"executable":bool,"reason":"short"}.`

type StepEvaluationResult struct {
	Executable bool   `json:"executable"`
	Reason     string `json:"reason"`
}

type EvaluateStepInput struct {
	Step state.PlanStep
}

type EvaluateStepPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewEvaluateStepPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *EvaluateStepPrimitive {
	return &EvaluateStepPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *EvaluateStepPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in EvaluateStepInput) (StepEvaluationResult, error) {
	stepJSONBytes, _ := json.Marshal(in.Step)
	purpose := "STEP:\n" + string(stepJSONBytes)

	req := PromptRequest{
		PrimitiveInstructions: stepEvalPrompt,
		PrimitiveInput:        purpose,
		Step:                  &in.Step,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out StepEvaluationResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, func() error {
		return validateStepEvaluation(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("EvaluateStep", false)
		} else {
			pc.RecordInference("EvaluateStep", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "EvaluateStep", "evaluate plan step", user, a.Raw)
	}

	return out, err
}

func validateStepEvaluation(v *StepEvaluationResult) error {
	if v.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}
