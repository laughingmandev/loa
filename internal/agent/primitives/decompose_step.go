package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type StepDecomposition struct {
	Steps  []DraftStep `json:"steps"`
	Reason string      `json:"reason"`
}

type DecomposeStepInput struct {
	StepJSON  string
	MayModify bool
	Eval      string
}

type DecomposeStepPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewDecomposeStepPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *DecomposeStepPrimitive {
	return &DecomposeStepPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var DecomposeStepPrompt = `Split the current step into smaller ordered steps, each with one coherent objective and independently verifiable expected result. If the original step was a broad investigation, split it by component, file, or distinct logical flow.
TASK MODIFICATION AUTHORIZATION is a hard capability boundary. If the active task has may_modify=false, decomposition must contain only investigate/verify steps and must not introduce modifying work.
Allowed mode values are exactly: investigate, modify, verify.
Allowed expected_tools values are exactly: read_file|read_range|search_path|search_text|find_symbol|list_symbols|read_ast_node|write_file|patch_file|patch_ast_node|create_directory|delete_file|delete_directory|execute_process|execute_shell|git_status|git_diff|read_tool_output|analyze_large_file|search_task_steps|read_task_step.
expected_tools contains Loa tool kinds, NOT executable/program names or shell aliases. For example use "execute_process" or "execute_shell", never "go", "bash", "shell", "mkdir", "docker", or "docker-compose". An empty expected_tools list is valid when no specific tool is known yet.
Max Prerequisites Rule: A single step MAY NOT depend on more than %d previous steps. If you need to integrate more things, create intermediate synthesis steps.
Return {"steps":[{"title":"","goal":"","reason":"","expected_result":"","mode":"modify","depends_on":[],"expected_tools":["write_file"]}],"reason":"short"}. depends_on uses 1-based positions and may only reference earlier steps in this decomposition.`

func (p *DecomposeStepPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in DecomposeStepInput) (StepDecomposition, error) {
	modAuth := "false"
	if in.MayModify {
		modAuth = "true"
	}
	purpose := "STEP:\n" + in.StepJSON + "\n\nTASK MODIFICATION AUTHORIZATION: " + modAuth + "\n\nEVALUATION:\n" + in.Eval

	req := PromptRequest{
		PrimitiveInstructions: fmt.Sprintf(DecomposeStepPrompt, p.cfg.MaxPrerequisitesPerStep),
		PrimitiveInput:        purpose,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out StepDecomposition
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, user, &out, func() error {
		return ValidatePlanForTask(&PlanResult{Steps: out.Steps}, in.MayModify, p.cfg.MaxPrerequisitesPerStep)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("DecomposeStep", false)
		} else {
			pc.RecordInference("DecomposeStep", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "DecomposeStep", "decompose step", user, a.Raw)
	}

	return out, err
}
