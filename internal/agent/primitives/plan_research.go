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

// PlanResearchDecision is one action of the pre-planning research loop.
// Note: Attachment reading uses a dedicated "read_attachment" action here instead of the general
// "tool" (read_file/read_range) action used in ExecuteStep. Advertising read_file in pre-planning
// tempts the model to inspect arbitrary project source files before a plan exists. Keeping a
// separate action (and only injecting it into the prompt when attachments are present) prevents
// premature codebase exploration while routing the underlying read through executeTool for buffer control.
type PlanResearchDecision struct {
	Action       string   `json:"action"`
	Reason       string   `json:"reason"`
	Query        string   `json:"query,omitempty"`
	AllowedKinds []string `json:"allowed_kinds,omitempty"`
	Offset       int      `json:"offset,omitempty"`
	Count        int      `json:"count,omitempty"`
	Path         string   `json:"path,omitempty"`
	Start        int      `json:"start,omitempty"`
	End          int      `json:"end,omitempty"`
	StartLine    int      `json:"start_line,omitempty"`
	EndLine      int      `json:"end_line,omitempty"`
}

type PlanResearchInput struct {
	Purpose        string
	Extras         []string
	HasAttachments bool
}

type PlanResearchPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewPlanResearchPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *PlanResearchPrimitive {
	return &PlanResearchPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

// buildPlanResearchPrompt constructs the pre-planning research instructions.
// We intentionally omit "read_attachment" unless the session actually has active attachments, and we
// never expose general codebase tools like "read_file" here so the model does not attempt to browse
// project files during pre-planning.
func buildPlanResearchPrompt(hasAttachments bool) string {
	var b strings.Builder
	b.WriteString("You are preparing to write the acceptance criteria and the plan for the task. Before planning, collect only the information the planner needs and does not have yet:\n")
	b.WriteString("- what the user asked for, suggested, agreed on or rejected in the chat history (the chat history is NOT in your context; use read_history),\n")
	if hasAttachments {
		b.WriteString("- the content of attached files listed under USER ATTACHMENTS (use read_attachment with the exact listed path; omit start_line/end_line or set them to 0 to read the full file),\n")
	}
	b.WriteString("- relevant memories and past task summaries.\n")
	b.WriteString("Do NOT investigate the codebase. Reading or searching project source code belongs in the investigate steps of the plan, not here.\n")
	b.WriteString("Collected results appear in your context after each action. Do not repeat an action whose result is already there.\n")
	b.WriteString("Return ready_to_plan as soon as you have enough to write concrete acceptance criteria and a concrete plan. If the task is self-contained and nothing needs to be collected, return ready_to_plan immediately.\n")
	b.WriteString("For read_history: offset 0 is the newest message, higher offsets go further back; count is optional and capped by the system. State in reason exactly what you are looking for, because only the information relevant to that reason is returned.\n")
	b.WriteString("Return exactly one of:\n")
	b.WriteString(`{"action":"read_history","reason":"what you are looking for in the chat history","offset":0,"count":10}` + "\n")
	if hasAttachments {
		b.WriteString(`{"action":"read_attachment","reason":"short","path":"attachment/path/as/listed","start_line":0,"end_line":0}` + "\n")
	}
	b.WriteString(`{"action":"retrieve_memory","reason":"short","query":"semantic search phrase","allowed_kinds":["task_summary","fact"]}` + "\n")
	b.WriteString(`{"action":"task_summaries","reason":"short"}` + "\n")
	b.WriteString(`{"action":"ready_to_plan","reason":"short summary of what was collected"}`)
	return b.String()
}

func ValidatePlanResearch(d *PlanResearchDecision) error {
	if err := NeedReason(d.Reason); err != nil {
		return err
	}
	switch d.Action {
	case "ready_to_plan", "task_summaries":
	case "retrieve_memory":
		if strings.TrimSpace(d.Query) == "" {
			return errors.New("query required")
		}
	case "read_history":
		if d.Offset < 0 {
			return errors.New("offset must be >= 0 (0 is the newest message)")
		}
	case "read_attachment":
		if d.Start == 0 && d.StartLine != 0 {
			d.Start = d.StartLine
		}
		if d.End == 0 && d.EndLine != 0 {
			d.End = d.EndLine
		}
		if strings.TrimSpace(d.Path) == "" {
			return errors.New("path required for read_attachment")
		}
		if d.Start < 0 || d.End < 0 {
			return errors.New("start_line and end_line must be >= 0")
		}
		if d.End > 0 && d.Start > d.End {
			return errors.New("end_line must be >= start_line")
		}
	default:
		return fmt.Errorf("invalid plan research action %q; allowed values: read_history, read_attachment, retrieve_memory, task_summaries, ready_to_plan", d.Action)
	}
	return nil
}

func (p *PlanResearchPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in PlanResearchInput) (PlanResearchDecision, error) {
	req := PromptRequest{
		PrimitiveInstructions: buildPlanResearchPrompt(in.HasAttachments),
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out PlanResearchDecision
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidatePlanResearch(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("PlanResearch", false)
		} else {
			pc.RecordInference("PlanResearch", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "PlanResearch", "pre-planning research", user, a.Raw)
	}

	return out, err
}
