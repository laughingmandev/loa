package primitives

import (
	"context"
	"encoding/json"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var generateStepArtifactPrompt = `Analyze the step execution log. If you uncovered dense engineering data (endpoint mappings, database schemas, raw log analysis) that is too large for short-term facts but essential for final synthesis, compile it into a detailed markdown document. This will be saved as an artifact for later retrieval. If no such dense data exists, return an empty string. Return {"artifact_payload":"..."}.`

type GenerateStepArtifactResult struct {
	ArtifactPayload string `json:"artifact_payload"`
}

type GenerateStepArtifactInput struct {
	Step   state.PlanStep
	Extras []string
}

type GenerateStepArtifactPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewGenerateStepArtifactPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *GenerateStepArtifactPrimitive {
	return &GenerateStepArtifactPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *GenerateStepArtifactPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in GenerateStepArtifactInput) (GenerateStepArtifactResult, error) {
	stepJSONBytes, _ := json.Marshal(in.Step)
	purpose := "COMPLETED STEP:\n" + string(stepJSONBytes)

	req := PromptRequest{
		PrimitiveInstructions: generateStepArtifactPrompt,
		PrimitiveInput:        purpose,
		Step:                  &in.Step,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out GenerateStepArtifactResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, nil)

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("GenerateStepArtifact", false)
		} else {
			pc.RecordInference("GenerateStepArtifact", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "GenerateStepArtifact", "generate step artifact", a.Prompt, a.Raw)
	}

	return out, err
}
