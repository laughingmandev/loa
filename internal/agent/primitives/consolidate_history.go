package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

// ConsolidateHistoryResult is the information extracted from a range of chat messages.
type ConsolidateHistoryResult struct {
	Extract string `json:"extract"`
}

// ConsolidateHistoryInput carries a raw chat transcript and the reason the agent wants to read it.
type ConsolidateHistoryInput struct {
	Reason     string // why the agent requested this history range
	RangeLabel string // human-readable description of the range, e.g. "CHAT MESSAGES (offsets 0-9 of 42, 0 = newest)"
	Transcript string // messages oldest-first, one line each
}

type ConsolidateHistoryPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewConsolidateHistoryPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ConsolidateHistoryPrimitive {
	return &ConsolidateHistoryPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var ConsolidateHistoryPrompt = `You are the chat history consolidation step. The agent requested a range of past chat messages for the stated reason for reading. Write a consolidated summary of what these messages say about that reason, for the agent to use as context. Do not copy the messages.
Follow these rules:
1. Include only information relevant to the reason for reading. Leave out everything else (other topics, pleasantries, explanations not needed for the reason).
2. Write self-contained statements: facts, decisions, requirements, constraints, agreed suggestions. Prefix each statement with the timestamp of the message it is based on.
3. Lists are complete: if the user agreed to a list of items (suggestions, enhancements, requirements), every item becomes its own statement. Never drop or merge items. Each item may be shortened to its essential content.
4. Superseding: if a later message changes or replaces an earlier statement about the same thing, output only the current version. The result must contain no contradicting statements. Newer information supersedes older information only when both concern the same thing.
5. Participants: user statements are requirements. Agent statements are proposals; they count as agreed only if the user accepted them in a later message (for example "ok, implement it as you suggested"). Otherwise mark them as agent proposals.
6. Unresolvable conflict: if two user statements about the same thing conflict and their order or scope does not resolve it, do not pick one. State the conflict explicitly as one statement so the agent can ask the user.
7. History is the past: the statements describe what was said at that time. They are not evidence of the current state of the code or the system.
8. Do not add information that is not in the messages. A message containing "[THIS MESSAGE WAS MOVED TO TASK OBJECTIVE]" is a timeline marker for the current task's prompt and must be ignored. If nothing in the messages is relevant to the reason for reading, say exactly that.
Return {"extract":"..."}.`

func ValidateConsolidateHistory(v *ConsolidateHistoryResult) error {
	if strings.TrimSpace(v.Extract) == "" {
		return errors.New("extract required")
	}
	return nil
}

func (p *ConsolidateHistoryPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ConsolidateHistoryInput) (ConsolidateHistoryResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: ConsolidateHistoryPrompt,
		PrimitiveInput:        "REASON FOR READING:\n" + in.Reason + "\n\n" + in.RangeLabel + ":\n" + in.Transcript,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out ConsolidateHistoryResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateConsolidateHistory(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ConsolidateHistory", false)
		} else {
			pc.RecordInference("ConsolidateHistory", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ConsolidateHistory", "consolidate chat history", user, a.Raw)
	}

	return out, err
}
