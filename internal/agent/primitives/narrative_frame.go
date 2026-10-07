package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type NarrativeFrameResult struct {
	Frame string `json:"frame"`
}

type NarrativeFrameInput struct {
	Purpose string
}

type NarrativeFramePrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewNarrativeFramePrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *NarrativeFramePrimitive {
	return &NarrativeFramePrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var NarrativeFramePrompt = `Analyze the provided code chunk and generate a concise Narrative Frame explaining its structural purpose, key responsibilities, and relationships to other components.
Return {"frame":"..."}.`

func ValidateNarrativeFrame(v *NarrativeFrameResult) error {
	if strings.TrimSpace(v.Frame) == "" {
		return errors.New("frame is required")
	}
	return nil
}

func (p *NarrativeFramePrimitive) Execute(ctx context.Context, pc PrimitiveContext, in NarrativeFrameInput) (NarrativeFrameResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: NarrativeFramePrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out NarrativeFrameResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelCrawling, p.baseSystem, user, &out, func() error {
		return ValidateNarrativeFrame(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("NarrativeFrame", false)
		} else {
			pc.RecordInference("NarrativeFrame", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "NarrativeFrame", "generate narrative frame", user, a.Raw)
	}

	return out, err
}
