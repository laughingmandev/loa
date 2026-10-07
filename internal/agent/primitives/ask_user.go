package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type AskResult struct {
	Question string `json:"question"`
}

type AskUserInput struct {
	Purpose string
}

type AskUserPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewAskUserPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *AskUserPrimitive {
	return &AskUserPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var AskPrompt = `Formulate one concise clarification question for the user that resolves the supplied missing information. Return {"question":"..."}.`

func ValidateAskResult(v *AskResult) error {
	if strings.TrimSpace(v.Question) == "" {
		return errors.New("question required")
	}
	return nil
}

func (p *AskUserPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in AskUserInput) (AskResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: AskPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out AskResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, user, &out, func() error {
		return ValidateAskResult(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("AskUser", false)
		} else {
			pc.RecordInference("AskUser", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "AskUser", "ask user", user, a.Raw)
	}

	return out, err
}
