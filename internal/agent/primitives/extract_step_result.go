package primitives

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var extractStepPrompt = `Extract only durable information future plan steps need from the completed step. If any artifacts were created or modified during the step via artifact tools, list their exact filenames in "artifacts". Return {"summary":"short","facts_learned":["..."],"decisions":["..."],"artifacts":["..."]}.`

type StepExtractResult struct {
	Summary      string   `json:"summary"`
	FactsLearned []string `json:"facts_learned,omitempty"`
	Decisions    []string `json:"decisions,omitempty"`
	Artifacts    []string `json:"artifacts,omitempty"`
}

type ExtractStepResultInput struct {
	Step   state.PlanStep
	Extras []string
	IsPartial bool
}

type ExtractStepResultPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewExtractStepResultPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ExtractStepResultPrimitive {
	return &ExtractStepResultPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *ExtractStepResultPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ExtractStepResultInput) (StepExtractResult, error) {
	stepJSONBytes, _ := json.Marshal(in.Step)
	prefix := "PARTIAL STEP PROGRESS:\n"
	if !in.IsPartial {
		prefix = "COMPLETED STEP:\n"
	}
	purpose := prefix + string(stepJSONBytes)

	req := PromptRequest{
		PrimitiveInstructions: extractStepPrompt,
		PrimitiveInput:        purpose,
		Step:                  &in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out StepExtractResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return validateStepExtract(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ExtractStepResult", false)
		} else {
			pc.RecordInference("ExtractStepResult", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ExtractStepResult", "extract step result", user, a.Raw)
	}

	return out, err
}

func validateStepExtract(v *StepExtractResult) error {
	if strings.TrimSpace(v.Summary) == "" {
		return errors.New("summary required")
	}
	return nil
}
