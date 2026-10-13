package primitives

import (
	"context"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

const criteriaPrompt = `Define the stable acceptance criteria for this coding task: the concrete conditions that must be true before the task may be called complete.
Use the original task, relevant prior discussion/context, and any existing acceptance criteria supplied in the input. Preserve existing criteria unless the latest user request explicitly corrects, removes, or adds a requirement. If existing acceptance criteria contain numeric id fields, those ids are Loa-internal identifiers and are NOT references to numbered items or ordinal phrases in the user's conversation. Focus on user-visible/product outcomes and required verification, not internal implementation steps. Do not promote optional future ideas into required scope. Do not invent requirements that are not supported by the supplied context.
Return {"criteria":["self-contained required outcome"],"reason":"short"}.`

type CriteriaResult struct {
	Criteria []string `json:"criteria"`
	Reason   string   `json:"reason"`
}

type CriteriaInput struct {
	Purpose string
	Extras  []string
}

type CriteriaPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewCriteriaPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *CriteriaPrimitive {
	return &CriteriaPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *CriteriaPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in CriteriaInput) (CriteriaResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: criteriaPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out CriteriaResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return validateCriteria(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("DefineAcceptanceCriteria", false)
		} else {
			pc.RecordInference("DefineAcceptanceCriteria", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "DefineAcceptanceCriteria", "define acceptance criteria", user, a.Raw)
	}

	return out, err
}

func validateCriteria(v *CriteriaResult) error {
	if v.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	if len(v.Criteria) == 0 {
		return fmt.Errorf("at least one acceptance criterion is required")
	}
	seen := map[string]bool{}
	for i, criterion := range v.Criteria {
		criterion = strings.TrimSpace(criterion)
		if criterion == "" {
			return fmt.Errorf("acceptance criterion %d is empty", i+1)
		}
		key := strings.ToLower(criterion)
		if seen[key] {
			return fmt.Errorf("duplicate acceptance criterion %q", criterion)
		}
		seen[key] = true
	}
	return nil
}
