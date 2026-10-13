package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var synthesizePrompt = `You have just completed a Fast Track execution loop. You did not use a rigid plan, but rather executed a series of actions dynamically.
Your task is to synthesize the work you accomplished into a final, conversational response for the user.

Review the original goal, the pinned evaluation (if any), the context trail of your actions, and any artifacts you created.
Summarize what was done, explain any important decisions, and highlight if anything remains incomplete.

Return exactly one of the following JSON structures:
{"response": "Your conversational summary of the work"}
`

type SynthesizeTaskResult struct {
	Response string `json:"response"`
}

type SynthesizeTaskInput struct {
	Goal             string
	PinnedEvaluation string
	Extras           []string
}

type SynthesizeTaskPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewSynthesizeTaskPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *SynthesizeTaskPrimitive {
	return &SynthesizeTaskPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *SynthesizeTaskPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in SynthesizeTaskInput) (SynthesizeTaskResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: synthesizePrompt,
		PrimitiveInputLabel:   "\nORIGINAL GOAL:\n",
		PrimitiveInput:        in.Goal,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
		PinnedEvaluation:      in.PinnedEvaluation,
	}
	var user string
	_ = user

	var out SynthesizeTaskResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		if out.Response == "" {
			return fmt.Errorf("response is required")
		}
		return nil
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("SynthesizeTask", false)
		} else {
			pc.RecordInference("SynthesizeTask", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "SynthesizeTask", "fast loop synthesis", a.Prompt, a.Raw)
	}

	return out, err
}
