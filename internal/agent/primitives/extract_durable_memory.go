package primitives

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type MemoryDraft struct {
	Kind        state.MemoryKind    `json:"kind"`
	Text        string              `json:"text"`
	Participant state.MessageSource `json:"participant"`
}

type MemoryExtraction struct {
	Items []MemoryDraft `json:"items"`
}

type ExtractDurableMemoryInput struct {
	Purpose string
}

type ExtractDurableMemoryPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewExtractDurableMemoryPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ExtractDurableMemoryPrimitive {
	return &ExtractDurableMemoryPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var ExtractDurableMemoryPrompt = `Analyze the most recent message from the user in the provided interaction. Extract new durable facts, decisions, corrections, or user approvals ONLY if they were introduced in this specific newest message. 
Use the older messages strictly as context to understand the new message (for example, to know what "okay" or "do that" refers to). Do not extract facts that were already established in the previous messages.
Do not store pleasantries or transient chatter.
Do NOT extract the user's immediate task instructions or commands (e.g. "analyze this file", "fix this bug"). Only extract persistent user constraints, preferences, or environment details.
Allowed kind values are exactly: fact, decision, approval, step_result.
Return {"items":[{"kind":"fact","text":"self-contained statement"}]}. An empty items list is valid.`

func ValidateMemory(m *MemoryExtraction) error {
	for i := range m.Items {
		switch m.Items[i].Kind {
		case state.MemoryFact, state.MemoryDecision, state.MemoryApproval, state.MemoryStepResult, state.MemoryStructural, state.MemoryGlobalConcept:
		default:
			return fmt.Errorf("invalid memory kind %q; allowed values: fact, decision, approval, step_result, structural, global_concept", m.Items[i].Kind)
		}
		if strings.TrimSpace(m.Items[i].Text) == "" {
			return errors.New("memory text required")
		}
	}
	return nil
}

func (p *ExtractDurableMemoryPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ExtractDurableMemoryInput) (MemoryExtraction, error) {
	req := PromptRequest{
		PrimitiveInstructions: ExtractDurableMemoryPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out MemoryExtraction
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelCrawling, p.baseSystem, user, &out, func() error {
		return ValidateMemory(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ExtractDurableMemory", false)
		} else {
			pc.RecordInference("ExtractDurableMemory", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ExtractDurableMemory", "extract durable memory", user, a.Raw)
	}

	return out, err
}
