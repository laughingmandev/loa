package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var TaskContextPrompt = `Refresh the active task context after a completed step. Keep only the compact current truth future steps need. Explicit user guidance is separately preserved verbatim in active task state; do not contradict or silently discard it. Preserve still-valid facts, decisions, constraints, and unresolved issues. Remove or replace facts that the new step evidence clearly supersedes; do not keep both an obsolete state and its newer replacement. Do not invent facts.
You MUST remove open issues that have been answered or resolved. Do not blindly preserve them if the new step evidence addresses them.
When carrying forward source-derived literals, preserve them exactly. Never paraphrase, normalize, merge, or creatively rewrite code identifiers, filenames, paths, command names, enum/status values, quoted output fragments, numeric values, ports, or bracketed markers such as [ON]/[OFF]. If an exact literal is uncertain, omit it rather than mutate it.
Return {"facts":["..."],"decisions":["..."],"constraints":["..."],"open_issues":["..."]}. Empty arrays are valid.`

type RefreshTaskContextInput struct {
	Purpose string
	Step    *state.PlanStep
}

type RefreshTaskContextPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewRefreshTaskContextPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *RefreshTaskContextPrimitive {
	return &RefreshTaskContextPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *RefreshTaskContextPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in RefreshTaskContextInput) (state.TaskContext, error) {
	var refreshed state.TaskContext
	req := PromptRequest{
		PrimitiveInstructions: TaskContextPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  in.Step,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	attempts, err := p.llm.ChatJSON(
		ctx,
		p.cfg.ModelExecuting,
		p.baseSystem,
		func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	},
		&refreshed,
		func() error { return validateTaskContext(&refreshed) },
	)

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("RefreshTaskContext", false)
		} else {
			pc.RecordInference("RefreshTaskContext", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "RefreshTaskContext", "refresh task context", in.Purpose, a.Raw)
	}

	return refreshed, err
}

func validateTaskContext(v *state.TaskContext) error {
	for _, group := range [][]string{v.Facts, v.Decisions, v.Constraints, v.OpenIssues} {
		for _, item := range group {
			if strings.TrimSpace(item) == "" {
				return errors.New("task context entries must not be empty")
			}
		}
	}
	return nil
}
