package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
)

const synthesizeTargetPrompt = `You are the Target Synthesizer.
Your goal is to evaluate the task and condense its primary objective into a short, concise $TARGET string.
This $TARGET will be prepended to all subsequent planning and execution prompts to keep the agent focused and prevent scope creep.

Rules:
1. The target should be a highly concise sentence fragment (e.g. "Focus: internal/llm/client.go and retry logic").
2. Identify the core files, components, or the single primary outcome.
3. Keep it under 20 words.
4. Return a valid JSON object with the "target" field.

Return {"target": "..."}`

type SynthesizeTargetInput struct {
	Goal     string
	Purpose  string
}

type SynthesizeTargetResult struct {
	Target string `json:"target"`
}

type SynthesizeTargetPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewSynthesizeTargetPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *SynthesizeTargetPrimitive {
	return &SynthesizeTargetPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *SynthesizeTargetPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in SynthesizeTargetInput) (string, error) {
	req := PromptRequest{
		PrimitiveInstructions: synthesizeTargetPrompt,
		PrimitiveInputLabel:   "\nTASK GOAL:\n",
		PrimitiveInput:        in.Goal + "\n\nADDITIONAL CONTEXT:\n" + in.Purpose,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out SynthesizeTargetResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		if strings.TrimSpace(out.Target) == "" {
			return errors.New("target is required")
		}
		return nil
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("SynthesizeTarget", false)
		} else {
			pc.RecordInference("SynthesizeTarget", true)
		}
	}

	return strings.TrimSpace(out.Target), err
}
