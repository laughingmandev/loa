package primitives

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var planEvalPrompt = `Evaluate whether this plan is logically ordered, sufficiently complete, covers the supplied acceptance criteria, and is suitable for execution. Focus on missing prerequisites, verification, and obviously oversized or vague steps. Fine-grained step decomposition will also happen just before each step.
Acceptance criterion id values are Loa-internal identifiers assigned after requirement extraction. They are NOT references to numbered items, ordinal phrases, or labels in the user's conversation. Judge coverage from each criterion's requirement text.
Return exactly one coverage entry for every supplied acceptance criterion, mapped to the ACTUAL plan step ids supplied in PLAN. A criterion may map to multiple steps when implementation and verification are separate. If no plan step covers a criterion, use an empty step_ids array and valid MUST be false. Do not attach an unrelated step merely to make the mapping non-empty. valid=true is allowed only when every acceptance criterion has one or more genuinely relevant covering steps.
Return {"valid":bool,"reason":"short","coverage":[{"criterion_id":1,"step_ids":[101,102],"reason":"how these steps cover the criterion"}],"issues":["..."]}.`

type CriterionCoverage struct {
	CriterionID uint64   `json:"criterion_id"`
	StepIDs     []uint64 `json:"step_ids"`
	Reason      string   `json:"reason"`
}

type PlanEvaluation struct {
	Valid    bool                `json:"valid"`
	Reason   string              `json:"reason"`
	Coverage []CriterionCoverage `json:"coverage"`
	Issues   []string            `json:"issues,omitempty"`
}

type EvaluatePlanInput struct {
	Purpose  string
	Extras   []string
	Criteria []state.AcceptanceCriterion
	Plan     state.Plan
}

type EvaluatePlanPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewEvaluatePlanPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *EvaluatePlanPrimitive {
	return &EvaluatePlanPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *EvaluatePlanPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in EvaluatePlanInput) (PlanEvaluation, error) {
	var ev PlanEvaluation
	b, _ := json.Marshal(in.Plan)
	fullInput := fmt.Sprintf("%s\n\nPLAN DRAFT:\n%s", in.Purpose, string(b))

	req := PromptRequest{
		PrimitiveInstructions: planEvalPrompt,
		PrimitiveInput:        fullInput,
		Step:                  nil,
		Extras:                in.Extras,
		IncludeInstructions:   true,
		GlobalPlanView:        false,
	}
	user := pc.BuildPrompt(req)

	attempts, err := p.llm.ChatJSON(
		ctx,
		p.cfg.ModelPlanning,
		p.baseSystem,
		user,
		&ev,
		func() error { return ValidatePlanEvaluation(&ev, in.Criteria, in.Plan) },
	)

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("EvaluatePlan", false)
		} else {
			pc.RecordInference("EvaluatePlan", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "EvaluatePlan", "evaluate plan", in.Purpose, a.Raw)
	}

	return ev, err
}

func ValidatePlanEvaluation(v *PlanEvaluation, criteria []state.AcceptanceCriterion, plan state.Plan) error {
	if err := NeedReason(v.Reason); err != nil {
		return err
	}

	criteriaByID := make(map[uint64]state.AcceptanceCriterion, len(criteria))
	for _, criterion := range criteria {
		criteriaByID[criterion.ID] = criterion
	}
	stepsByID := make(map[uint64]state.PlanStep, len(plan.Steps))
	for _, step := range plan.Steps {
		stepsByID[step.ID] = step
	}

	seenCriteria := make(map[uint64]bool, len(v.Coverage))
	for _, coverage := range v.Coverage {
		if _, ok := criteriaByID[coverage.CriterionID]; !ok {
			return fmt.Errorf("coverage entry references unknown acceptance criterion id %d", coverage.CriterionID)
		}
		if seenCriteria[coverage.CriterionID] {
			return fmt.Errorf("acceptance criterion id %d appears more than once in coverage", coverage.CriterionID)
		}
		seenCriteria[coverage.CriterionID] = true
		if err := NeedReason(coverage.Reason); err != nil {
			return fmt.Errorf("coverage for acceptance criterion id %d requires a reason", coverage.CriterionID)
		}

		seenSteps := map[uint64]bool{}
		for _, stepID := range coverage.StepIDs {
			if _, ok := stepsByID[stepID]; !ok {
				return fmt.Errorf("coverage for acceptance criterion id %d references unknown plan step id %d", coverage.CriterionID, stepID)
			}
			if seenSteps[stepID] {
				return fmt.Errorf("coverage for acceptance criterion id %d repeats plan step id %d", coverage.CriterionID, stepID)
			}
			seenSteps[stepID] = true
		}
	}

	for _, criterion := range criteria {
		var mapped *CriterionCoverage
		for i := range v.Coverage {
			if v.Coverage[i].CriterionID == criterion.ID {
				mapped = &v.Coverage[i]
				break
			}
		}
		if mapped == nil {
			return fmt.Errorf("plan evaluation is missing coverage for acceptance criterion id %d; include every supplied criterion, using an empty step_ids array when it is uncovered", criterion.ID)
		}
		if v.Valid && len(mapped.StepIDs) == 0 {
			return fmt.Errorf("valid plan leaves acceptance criterion id %d uncovered; provide one or more actual plan step ids or set valid=false", criterion.ID)
		}
	}
	return nil
}
