package primitives

import (
	"context"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/llm"
)

type TargetTag struct {
	StepID   int      `json:"step_id"`
	Entities []string `json:"entities"`
}

type PlanTargets struct {
	Tags []TargetTag `json:"tags"`
}

type TagTargetsInput struct {
	Purpose string
}

type TagTargetsPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewTagTargetsPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *TagTargetsPrimitive {
	return &TagTargetsPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var TagTargetsPrompt = `Analyze the provided plan draft and identify the exact target entities (files, classes, interfaces, or directories) that each step will likely read, modify, or investigate.
If a step modifies a specific file, include the filename. If a step analyzes a specific component, include its name.
These target entities will act as hard filters for structural memory lookup before execution begins.
Return {"tags":[{"step_id": 1, "entities":["main.go", "config"]}]}.`

func (p *TagTargetsPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in TagTargetsInput) (PlanTargets, error) {
	req := PromptRequest{
		PrimitiveInstructions: TagTargetsPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out PlanTargets
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, user, &out, nil)

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
			pc.RecordInference("TagTargets", a.PromptKind == "repair")
		pc.Log(kind, "TagTargets", "tag targets", user, a.Raw)
	}

	return out, err
}
