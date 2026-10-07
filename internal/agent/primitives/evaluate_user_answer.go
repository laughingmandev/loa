package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type AnswerEvaluation struct {
	Sufficient     bool   `json:"sufficient"`
	Reason         string `json:"reason"`
	FollowupNeeded string `json:"followup_needed,omitempty"`
}

type EvaluateUserAnswerInput struct {
	Purpose string
}

type EvaluateUserAnswerPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewEvaluateUserAnswerPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *EvaluateUserAnswerPrimitive {
	return &EvaluateUserAnswerPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var AnswerEvalPrompt = `Evaluate whether the user's answer sufficiently resolves the pending clarification. If not, describe the remaining gap. Return {"sufficient":bool,"reason":"short","followup_needed":"..."}.`

func ValidateAnswerEvaluation(v *AnswerEvaluation) error {
	if strings.TrimSpace(v.Reason) == "" {
		return errors.New("reason is required")
	}
	return nil
}

func (p *EvaluateUserAnswerPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in EvaluateUserAnswerInput) (AnswerEvaluation, error) {
	req := PromptRequest{
		PrimitiveInstructions: AnswerEvalPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out AnswerEvaluation
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, user, &out, func() error {
		return ValidateAnswerEvaluation(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("EvaluateUserAnswer", false)
		} else {
			pc.RecordInference("EvaluateUserAnswer", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "EvaluateUserAnswer", "evaluate user answer", user, a.Raw)
	}

	return out, err
}
