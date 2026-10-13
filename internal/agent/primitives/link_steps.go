package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

// StepMapping maps a global receiver step to the new decomposed steps it depends on
type StepMapping struct {
	ReceiverID uint64   `json:"receiver_id"`
	DependsOn  []uint64 `json:"depends_on"`
}

type LinkStepsResult struct {
	Mappings           []StepMapping `json:"mappings,omitempty"`
	Reason             string        `json:"reason,omitempty"`
	Action             string        `json:"action,omitempty"`
	ReplanInstructions string        `json:"replan_instructions,omitempty"`
}

type LinkStepsInput struct {
	DecomposedSteps   []state.PlanStep
	OriginalStepID    uint64
	OriginalReceivers []uint64
	GlobalPlan        state.Plan
}

type LinkStepsPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewLinkStepsPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *LinkStepsPrimitive {
	return &LinkStepsPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var LinkStepsPrompt = `You are integrating newly decomposed steps back into the global execution plan.
Your task is to explicitly link the global steps that originally depended on the decomposed step (the "Original Receivers") to the new sub-steps.

RULES:
1. The new sub-steps are already internally linked to each other. You ONLY need to map the global receiver steps to the appropriate new sub-steps (usually the final synthesis/leaf steps of the new group).
2. Max Prerequisites Rule: A single step MAY NOT depend on more than %d previous steps in total. If mapping would exceed this, you must rethink the mapping or escalate to a replan.
3. You must provide a mappings array. Each entry specifies a global receiver step ID and the array of new step IDs it now depends on. You may also link to other pending steps if logically required.

Return format:
{"mappings":[{"receiver_id": 90, "depends_on": [121, 122]}], "reason": "short explanation"}

ESCALATION (REPLAN):
If you realize that these new steps structurally invalidate the remaining global plan, or if wiring them is impossible without breaking the %d dependencies limit, you MUST escalate by returning:
{"action":"replan", "reason":"...", "replan_instructions":"Clear explanation of why the plan is broken and how the planner should rebuild it."}`

func (p *LinkStepsPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in LinkStepsInput) (LinkStepsResult, error) {
	b, _ := json.Marshal(in.DecomposedSteps)
	receivers, _ := json.Marshal(in.OriginalReceivers)
	purpose := fmt.Sprintf("ORIGINAL STEP ID: %d\nORIGINAL RECEIVERS (IDs): %s\nNEWLY DECOMPOSED STEPS:\n%s", in.OriginalStepID, string(receivers), string(b))

	req := PromptRequest{
		PrimitiveInstructions: fmt.Sprintf(LinkStepsPrompt, p.cfg.MaxPrerequisitesPerStep, p.cfg.MaxPrerequisitesPerStep),
		PrimitiveInput:        purpose,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	var out LinkStepsResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		if out.Action == "replan" {
			return nil
		}

		mappingLookup := make(map[uint64][]uint64)
		for _, m := range out.Mappings {
			mappingLookup[m.ReceiverID] = m.DependsOn
		}

		mappedNewSteps := make(map[uint64]bool)

		// Pre-populate mappedNewSteps by looking at internal dependencies within the decomposed steps
		for _, ds := range in.DecomposedSteps {
			for _, dep := range ds.DependsOn {
				mappedNewSteps[dep] = true
			}
		}

		for _, s := range in.GlobalPlan.Steps {
			newDepsCount := len(s.DependsOn)
			hasOldDep := false
			for _, d := range s.DependsOn {
				if d == in.OriginalStepID {
					hasOldDep = true
					newDepsCount--
					break
				}
			}

			if mappedDeps, ok := mappingLookup[s.ID]; ok {
				newDepsCount += len(mappedDeps)
				for _, d := range mappedDeps {
					mappedNewSteps[d] = true
				}
			} else if hasOldDep {
				newDepsCount++
				mappedNewSteps[in.DecomposedSteps[len(in.DecomposedSteps)-1].ID] = true
			}

			if newDepsCount > p.cfg.MaxPrerequisitesPerStep {
				return fmt.Errorf("Validation Error: Mapping causes Step %d to have %d dependencies (max %d). Rethink mapping or trigger replan.", s.ID, newDepsCount, p.cfg.MaxPrerequisitesPerStep)
			}
		}

		for _, ds := range in.DecomposedSteps {
			isLastDecStep := ds.ID == in.DecomposedSteps[len(in.DecomposedSteps)-1].ID
			parentWasLastStep := len(in.OriginalReceivers) == 0
			if isLastDecStep && parentWasLastStep {
				continue
			}

			if !mappedNewSteps[ds.ID] {
				return fmt.Errorf("Validation Error: Decomposed step %d was not mapped to any receiver. Every step must have a receiver.", ds.ID)
			}
		}

		return nil
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("LinkSteps", false)
		} else {
			pc.RecordInference("LinkSteps", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "LinkSteps", "link decomposed steps", user, a.Raw)
	}

	return out, err
}
