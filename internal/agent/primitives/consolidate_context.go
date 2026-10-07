package primitives

import (
	"context"
	"errors"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type ConsolidationClassification string

const (
	ConsolidationSemanticFact          ConsolidationClassification = "semantic_fact"
	ConsolidationEpisodicFact          ConsolidationClassification = "episodic_fact"
	ConsolidationArchitecturalDecision ConsolidationClassification = "architectural_decision"
	ConsolidationImplementationDecision ConsolidationClassification = "implementation_decision"
)

type ConsolidatedMemory struct {
	Classification ConsolidationClassification `json:"classification"`
	Text           string                      `json:"text"`
}

type ConsolidateContextResult struct {
	Items []ConsolidatedMemory `json:"items"`
}

type ConsolidateContextInput struct {
	Purpose string
}

type ConsolidateContextPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewConsolidateContextPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ConsolidateContextPrimitive {
	return &ConsolidateContextPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var ConsolidateContextPrompt = `You are the memory consolidation pipeline. Your job is to filter the agent's transient working memory into durable, high-value session memory.
Classify each item from the working memory into one of four strict categories:
- "semantic_fact": Timeless, objective truths about the codebase architecture, APIs, data structures, and environment setup. (KEEP)
- "episodic_fact": Observations about specific actions taken during the task, tool outputs, test runs, or artifact creations (e.g. "I wrote file X", "tests passed"). (DROP)
- "architectural_decision": High-level design choices or standards established during the task that should apply to future code. (KEEP)
- "implementation_decision": Tactical choices made specifically for the immediate task (e.g. "I decided to name this variable X", "Use this snippet for the response"). (DROP)

Output the classified items. Only include items that are worth evaluating; you may omit obvious noise.
Return {"items":[{"classification":"semantic_fact","text":"self-contained statement"}]}. An empty items list is valid if nothing is worth saving.`

func ValidateConsolidation(v *ConsolidateContextResult) error {
	for i := range v.Items {
		switch v.Items[i].Classification {
		case ConsolidationSemanticFact, ConsolidationEpisodicFact, ConsolidationArchitecturalDecision, ConsolidationImplementationDecision:
		default:
			return fmt.Errorf("invalid consolidation classification %q", v.Items[i].Classification)
		}
		if v.Items[i].Text == "" {
			return errors.New("memory text required")
		}
	}
	return nil
}

func (p *ConsolidateContextPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ConsolidateContextInput) (ConsolidateContextResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: ConsolidateContextPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out ConsolidateContextResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelCrawling, p.baseSystem, user, &out, func() error {
		return ValidateConsolidation(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ConsolidateContext", false)
		} else {
			pc.RecordInference("ConsolidateContext", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ConsolidateContext", "consolidate working memory", user, a.Raw)
	}

	return out, err
}
