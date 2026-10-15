package primitives

import (
	"context"
	"errors"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type RerankResult struct {
	SynthesizedContext    string   `json:"synthesized_context,omitempty"`
	SelectedStructuralIDs []uint64 `json:"selected_structural_ids,omitempty"`
	NeedsUser             bool     `json:"needs_user"`
	ClarificationNeeded   string   `json:"clarification_needed,omitempty"`
	Reason                string   `json:"reason"`
}

type RerankMemoryInput struct {
	Purpose  string
	Step     *state.PlanStep
	ValidIDs map[uint64]bool
	Limit    int
}

type RerankMemoryPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewRerankMemoryPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *RerankMemoryPrimitive {
	return &RerankMemoryPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var RerankPrompt = `Analyze the candidate memory entries and synthesize a single, coherent context block that is directly applicable to the current goal/question.
Candidate objects contain timestamps, kinds, and participant tags (user/agent).
Follow these rules:
1. Resolve any apparent contradictions using scope, timestamps, and participant tags. Newer information supersedes older information only when they concern the same thing. User statements are hard constraints; agent statements are past hypotheses.
2. If authoritative applicability cannot be determined and conflicting user directives exist, set needs_user=true and state exactly what must be clarified.
3. If no clarification is needed, output a detailed, unified "synthesized_context" string that the agent can read. Preserve exact code structures or ABIs if present.
Return {"synthesized_context":"...", "needs_user":false,"clarification_needed":"","reason":"short"}.`

func ValidateRerank(rr *RerankResult, valid map[uint64]bool, limit int) error {
	if rr.NeedsUser && rr.ClarificationNeeded == "" {
		return errors.New("must provide clarification_needed if needs_user is true")
	}
	if !rr.NeedsUser && rr.SynthesizedContext == "" && len(rr.SelectedStructuralIDs) == 0 {
		return errors.New("must provide synthesized_context or selected_structural_ids if needs_user is false")
	}
	if len(rr.SelectedStructuralIDs) > limit {
		return errors.New("selected_structural_ids exceeds requested limit")
	}
	for _, id := range rr.SelectedStructuralIDs {
		if !valid[id] {
			return errors.New("selected_structural_ids contains invalid candidate ID")
		}
	}
	return nil
}

func (p *RerankMemoryPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in RerankMemoryInput) (RerankResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: RerankPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  in.Step,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out RerankResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateRerank(&out, in.ValidIDs, in.Limit)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("RerankMemory", false)
		} else {
			pc.RecordInference("RerankMemory", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "RerankMemory", "rerank memory", user, a.Raw)
	}

	return out, err
}
