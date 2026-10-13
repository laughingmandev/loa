package primitives

import (
	"context"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

var planPrompt = `Create a practical coding plan for the task. Make a rough complete plan, including investigative and verification steps. The plan must cover the supplied acceptance criteria. Steps should each have one coherent objective that can later be independently evaluated. Do not over-compress work into giant steps.
TASK MODIFICATION AUTHORIZATION in the supplied task state is a hard capability boundary. If may_modify=false, the plan must contain only investigate/verify steps and must not propose repository modifications.
Acceptance criterion id values are Loa-internal identifiers assigned after requirement extraction. They are NOT references to numbered items, ordinal phrases, or labels in the user's conversation. Judge each criterion by its requirement text, not by numeric coincidence.
Allowed mode values are exactly: investigate, modify, verify.
Allowed expected_tools values are exactly: ` + ToolKindEnum + `.
expected_tools contains Loa tool kinds, NOT executable/program names or shell aliases. For example use "execute_process" or "execute_shell", never "go", "bash", "shell", "mkdir", "docker", or "docker-compose". An empty expected_tools list is valid when no specific tool is known yet.
Max Prerequisites Rule: A single step MAY NOT depend on more than %d previous steps. If you need to integrate more things, create intermediate synthesis steps.
If the task is so ambiguous that a plan cannot be safely created without user clarification, you may use the ask_user action.
Return {"steps":[{"title":"","goal":"","reason":"","expected_result":"","mode":"modify","depends_on":[],"expected_tools":["write_file"]}],"reason":"short"}. depends_on uses 1-based positions and may only reference earlier steps.
OR, if clarification is strictly required: {"action":"ask_user","reason":"why","ask_user_question":"what to ask"}.`

type DraftStep struct {
	Title          string           `json:"title"`
	Goal           string           `json:"goal"`
	Reason         string           `json:"reason"`
	ExpectedResult string           `json:"expected_result"`
	Mode           state.StepMode   `json:"mode"`
	DependsOn      []int            `json:"depends_on,omitempty"`
	ExpectedTools  []state.ToolKind `json:"expected_tools,omitempty"`
	TargetEntities []string         `json:"target_entities,omitempty"`
}

type PlanResult struct {
	Action          string      `json:"action,omitempty"`
	AskUserQuestion string      `json:"ask_user_question,omitempty"`
	Steps           []DraftStep `json:"steps,omitempty"`
	Reason          string      `json:"reason"`
}

type PlanInput struct {
	Operation string // e.g. "PlanTask" or "Replan"
	Purpose   string
	Extras    []string
	MayModify bool
}

type PlanPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewPlanPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *PlanPrimitive {
	return &PlanPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *PlanPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in PlanInput) (PlanResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: fmt.Sprintf(planPrompt, p.cfg.MaxPrerequisitesPerStep),
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out PlanResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidatePlanForTask(&out, in.MayModify, p.cfg.MaxPrerequisitesPerStep)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference(in.Operation, false)
		} else {
			pc.RecordInference(in.Operation, true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, in.Operation, "plan task execution", user, a.Raw)
	}

	return out, err
}

func ValidatePlanForTask(p *PlanResult, mayModify bool, maxPrerequisites int) error {
	if err := ValidatePlan(p); err != nil {
		return err
	}
	if mayModify {
		// Even if they can modify, check max prerequisites limit
		for i, step := range p.Steps {
			if len(step.DependsOn) > maxPrerequisites {
				return fmt.Errorf("Validation Error: Step %d has %d dependencies, but the maximum allowed is %d. You must decompose this node and create intermediate synthesis steps.", i+1, len(step.DependsOn), maxPrerequisites)
			}
		}
		return nil
	}
	for i, step := range p.Steps {
		if step.Mode == state.StepModify {
			return fmt.Errorf("step %d requests modify mode but this task is strictly read-only", i+1)
		}
		for _, kind := range step.ExpectedTools {
			if tools.IsMutating(kind) && kind != state.ToolExecuteProcess && kind != state.ToolExecuteShell {
				return fmt.Errorf("step %d expects mutating tool %q but this task is strictly read-only", i+1, kind)
			}
		}
	}
	return nil
}

func ValidatePlan(p *PlanResult) error {
	if p.Action == "ask_user" {
		if strings.TrimSpace(p.AskUserQuestion) == "" {
			return fmt.Errorf("ask_user_question required")
		}
		return nil
	}
	if len(p.Steps) == 0 {
		return fmt.Errorf("plan requires at least one step or an ask_user action")
	}
	for i, s := range p.Steps {
		if strings.TrimSpace(s.Title) == "" || strings.TrimSpace(s.Goal) == "" || strings.TrimSpace(s.Reason) == "" || strings.TrimSpace(s.ExpectedResult) == "" {
			return fmt.Errorf("step %d missing required fields", i+1)
		}
		if s.Mode != state.StepInvestigate && s.Mode != state.StepModify && s.Mode != state.StepVerify {
			return fmt.Errorf("step %d invalid mode %q; allowed values: investigate, modify, verify", i+1, s.Mode)
		}
		for _, tk := range s.ExpectedTools {
			if !tools.ValidKind(tk) {
				return fmt.Errorf("step %d invalid expected tool %q", i+1, tk)
			}
		}
		for _, d := range s.DependsOn {
			if d < 1 || d >= i+1 {
				return fmt.Errorf("step %d has invalid dependency %d; dependencies must reference earlier steps", i+1, d)
			}
		}
	}
	return nil
}
