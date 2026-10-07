package primitives

import (
	"context"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

const intentPrompt = `Classify the user's latest message. Multiple intents may be present. "answers_pending_question" is contextual, not an intent.
Any direct instruction to change the project must include the task intent, even when it is also a correction. If a message mixes discussion/questioning with an unsettled implementation task, prefer discussion first. Mark ambiguous if the expected behavior is genuinely unclear.
Allowed intent values are exactly: statement, correction, question, discussion, task.
If the intent includes "task", you MUST provide a concise 3-5 word task_title summarizing the goal.
Return JSON: {"intents":["statement"],"task_title":"...","answers_pending_question":bool,"ambiguous":bool,"reason":"short"}`

type IntentInput struct {
	UserText string
}

type IntentPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewIntentPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *IntentPrimitive {
	return &IntentPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *IntentPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in IntentInput) (state.IntentResult, error) {
	purpose := "LATEST USER MESSAGE:\n" + in.UserText

	// Build context (no step, no extras, includeInstructions=false)
	req := PromptRequest{
		PrimitiveInstructions: intentPrompt,
		PrimitiveInput:        purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   false,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out state.IntentResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, user, &out, func() error {
		return validateIntent(&out)
	})
	
	for i := range attempts {
		if i == 0 {
			pc.RecordInference("InterpretIntent", false)
		} else {
			pc.RecordInference("InterpretIntent", true)
		}
	}
	
	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "InterpretIntent", "interpret user intent", user, a.Raw)
	}

	return out, err
}

func validateIntent(r *state.IntentResult) error {
	for _, i := range r.Intents {
		if i != "statement" && i != "correction" && i != "question" && i != "discussion" && i != "task" {
			return errorInvalidIntent(string(i))
		}
	}
	return nil
}

type intentError string

func errorInvalidIntent(i string) error {
	return intentError("invalid intent: " + i)
}
func (e intentError) Error() string { return string(e) }
