package primitives

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

type NegotiationDecision struct {
	Action             string         `json:"action"`
	Reason             string         `json:"reason"`
	Query              string         `json:"query,omitempty"`
	AllowedKinds       []string       `json:"allowed_kinds,omitempty"`
	Tool               *tools.Request `json:"tool,omitempty"`
	Answer             string         `json:"answer,omitempty"`
	Question           string         `json:"question,omitempty"`
	ReplanInstructions string         `json:"replan_instructions,omitempty"`
}

type NegotiationInput struct {
	Purpose string
	Extras  []string
}

type NegotiationPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewNegotiationPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *NegotiationPrimitive {
	return &NegotiationPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var NegotiationPrompt = `You are evaluating a proposed plan against user guidance that rejected or modified it. You must investigate or negotiate until you have enough information to approve the plan or replan it.
If the plan can be approved as-is based on new information, return {"action":"approve","reason":"..."}.
If the plan MUST be entirely replanned based on the guidance, return {"action":"replan","reason":"...","replan_instructions":"clear instructions for the planner"}.
If you need more information, return a discussion action (retrieve_memory, task_summaries, tool, ask_user).`

func ValidateNegotiation(d *NegotiationDecision) error {
	if d.Action == "approve" {
		if d.Reason == "" {
			return errors.New("reason is required")
		}
		return nil
	}
	if d.Action == "replan" {
		if d.Reason == "" {
			return errors.New("reason is required")
		}
		if strings.TrimSpace(d.ReplanInstructions) == "" {
			return errors.New("replan_instructions required for replan action")
		}
		return nil
	}
	// Validate discussion action
	if d.Reason == "" {
		return errors.New("reason is required")
	}
	switch d.Action {
	case "retrieve_memory":
		if strings.TrimSpace(d.Query) == "" {
			return errors.New("query required")
		}
	case "task_summaries":
	case "tool":
		if d.Tool == nil {
			return errors.New("tool request is required")
		}
		if d.Tool.Kind == "" {
			return errors.New("tool kind is required")
		}
		if d.Tool.Description == "" {
			d.Tool.Description = d.Reason
		}
	case "ask_user":
		if strings.TrimSpace(d.Question) == "" {
			return errors.New("question required")
		}
	case "final":
		if strings.TrimSpace(d.Answer) == "" {
			return errors.New("answer required")
		}
	default:
		if strings.Contains("read_file|read_range|search_path|search_text|find_symbol|list_symbols|read_ast_node|write_file|patch_file|patch_ast_node|create_directory|delete_file|delete_directory|execute_process|execute_shell|git_status|git_diff|read_tool_output|analyze_large_file|search_task_steps|read_task_step", d.Action) {
			return fmt.Errorf("action %q is a tool kind, not an action; set action=\"tool\", set tool.kind=%q, and preserve the existing tool.input", d.Action, d.Action)
		}
		return fmt.Errorf("invalid negotiation action %q; allowed values: retrieve_memory, task_summaries, tool, ask_user, final, approve, replan", d.Action)
	}
	return nil
}

func (p *NegotiationPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in NegotiationInput) (NegotiationDecision, error) {
	req := PromptRequest{
		PrimitiveInstructions: NegotiationPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out NegotiationDecision
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelConversation, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateNegotiation(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("Negotiation", false)
		} else {
			pc.RecordInference("Negotiation", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "Negotiation", "negotiation", user, a.Raw)
	}

	return out, err
}
