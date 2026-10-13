package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

const ambiguityPrompt = `The user's intent is ambiguous. Decide whether the message can safely be accepted as context without asking, or whether clarification is needed before proceeding.
Allowed action values are exactly: accept, ask_user.
Return {"action":"accept","reason":"short","missing":"what must be clarified, if any"}.`

type AmbiguityResult struct {
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Missing string `json:"missing,omitempty"`
}

type AmbiguityInput struct {
	UserText string
}

type AmbiguityPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewAmbiguityPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *AmbiguityPrimitive {
	return &AmbiguityPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *AmbiguityPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in AmbiguityInput) (AmbiguityResult, error) {
	purpose := "LATEST USER MESSAGE:\n" + in.UserText

	// Build context (no step, no extras, includeInstructions=false)
	req := PromptRequest{
		PrimitiveInstructions: ambiguityPrompt,
		PrimitiveInput:        purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out AmbiguityResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return validateAmbiguity(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ResolveAmbiguity", false)
		} else {
			pc.RecordInference("ResolveAmbiguity", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ResolveAmbiguity", "resolve ambiguity in user intent", user, a.Raw)
	}

	return out, err
}

func validateAmbiguity(v *AmbiguityResult) error {
	if v.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	if v.Action != "accept" && v.Action != "ask_user" {
		return fmt.Errorf("invalid ambiguity action %q; allowed values: accept, ask_user", v.Action)
	}
	if v.Action == "ask_user" && v.Missing == "" {
		return fmt.Errorf("missing string is required when asking user")
	}
	return nil
}
