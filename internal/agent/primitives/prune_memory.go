package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

const pruneMemoryPrompt = `You are the Memory Pruner.
Your goal is to actively garbage collect the agent's Working Memory to prevent context ballooning.
Review the current Working Memory (Facts and Decisions). Discard items that are:
1. Obsolete or resolved by completed steps.
2. Overly granular or transient (e.g., minor bugs that were immediately fixed).
3. Irrelevant to the remaining open steps in the Plan.

Return the JSON array of indices (0-based) for items that should be KEPT. Anything omitted will be deleted forever!
You must output a JSON object with two arrays: "keep_fact_indices" and "keep_decision_indices".

Format:
{
  "keep_fact_indices": [0, 2, 5],
  "keep_decision_indices": [1]
}`

type PruneMemoryInput struct {
	Facts     []string
	Decisions []string
	Plan      *state.Plan
}

type PruneMemoryResult struct {
	KeepFactIndices     []int `json:"keep_fact_indices"`
	KeepDecisionIndices []int `json:"keep_decision_indices"`
}

type PruneMemoryPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewPruneMemoryPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *PruneMemoryPrimitive {
	return &PruneMemoryPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *PruneMemoryPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in PruneMemoryInput) (PruneMemoryResult, error) {
	memJSON := "FACTS:\n"
	for i, f := range in.Facts {
		memJSON += fmt.Sprintf("[%d] %s\n", i, f)
	}
	memJSON += "\nDECISIONS:\n"
	for i, d := range in.Decisions {
		memJSON += fmt.Sprintf("[%d] %s\n", i, d)
	}

	planJSON := ""
	if in.Plan != nil {
		b, _ := json.Marshal(in.Plan)
		planJSON = string(b)
	}

	req := PromptRequest{
		PrimitiveInstructions: pruneMemoryPrompt,
		PrimitiveInputLabel:   "\nWORKING MEMORY:\n",
		PrimitiveInput:        memJSON + "\n\nCURRENT PLAN:\n" + planJSON,
		IncludeInstructions:   true,
		GlobalPlanView:        false, // The plan is already injected
		OmitChatHistory:       true,
	}
	var user string
	_ = user

	var out PruneMemoryResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return nil
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("PruneMemory", false)
		} else {
			pc.RecordInference("PruneMemory", true)
		}
	}

	return out, err
}
