package primitives

import (
	"context"

	"github.com/laughingmandev/loa/internal/state"
)

type PromptRequest struct {
	PrimitiveInstructions string
	PrimitiveInput        string
	PrimitiveInputLabel   string
	
	Step                  *state.PlanStep
	Extras                []string
	IncludeInstructions   bool
	GlobalPlanView        bool

	Target                string
	PinnedEvaluation      string
	BlindTrail            []string
	IncludeChatHistory    int
	OmitChatHistory       bool
	RepairReserve         int
}

// PrimitiveContext provides access to engine-level state, context building, and telemetry.
type PrimitiveContext interface {
	BuildPrompt(req PromptRequest) (string, error)
	RecordInference(primitive string, isRepair bool)
	Log(kind state.LogKind, primitive, summary, prompt, response string)
}

// Primitive defines a standardized node in the execution pipeline.
type Primitive[I any, O any] interface {
	Execute(ctx context.Context, pc PrimitiveContext, in I) (O, error)
}
