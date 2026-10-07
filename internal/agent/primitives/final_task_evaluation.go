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

type FinalCheck struct {
	Complete bool                    `json:"complete"`
	Reason   string                  `json:"reason"`
	Checks   []state.AcceptanceCheck `json:"checks"`
	Missing  []string                `json:"missing,omitempty"`
}

type FinalTaskEvaluationInput struct {
	Purpose    string
	Criteria   []state.AcceptanceCriterion
	IsReadOnly bool
}

type FinalTaskEvaluationPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewFinalTaskEvaluationPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *FinalTaskEvaluationPrimitive {
	return &FinalTaskEvaluationPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var ReadOnlyFinalCheckPrompt = `Independently decide whether the original investigatory/read-only task is actually complete. Evaluate every supplied acceptance criterion against the completed step results and artifacts.
Allowed check status values are exactly: verified, partial, unverified.
- verified means the required information (e.g., code snippet, explanation, or data) was successfully gathered, drafted, and recorded in the step results or an artifact.
- partial means some information was gathered but it is incomplete.
- unverified means the required information was not found or not recorded.
Unlike modifying tasks, read-only tasks do not require concrete codebase diffs or execution logs. A clear statement in the step results that the artifact was drafted or the answer was found is sufficient evidence.
Return {"complete":bool,"reason":"short","checks":[{"criterion_id":1,"status":"verified","evidence":["recorded in artifact X"],"missing":""}],"missing":["remaining gap"]}.`

var FinalCheckPrompt = `Independently decide whether the original task is actually complete. Evaluate every supplied acceptance criterion against the actual completed step results, concrete tool evidence, and prior verification concerns. Do not trust planner or executor completion claims blindly.
Allowed check status values are exactly: verified, partial, unverified.
- verified requires concrete supporting evidence.
- partial means some evidence exists but the criterion is not fully demonstrated.
- unverified means evidence is absent or insufficient.
Prior verification concerns are not automatically failures, but they remain unresolved until later concrete evidence demonstrates that the concern was corrected, disproven, or irrelevant. If a prior concern names a visible defect and later read/diff evidence still contains that defect, any affected criterion must not be marked verified. Do not let a compressed step summary override contradictory concrete evidence.
Every acceptance criterion must have exactly one check. Include short evidence references such as step results, files changed, command/test results, observed file content, or diffs. If partial/unverified, state what remains missing. complete=true is allowed only when every acceptance criterion is verified.
Return {"complete":bool,"reason":"short","checks":[{"criterion_id":1,"status":"verified","evidence":["concrete evidence"],"missing":""}],"missing":["remaining gap"]}.`

func ValidateFinalCheck(v *FinalCheck, criteria []state.AcceptanceCriterion) error {
	if v.Reason == "" {
		return errors.New("reason is required")
	}
	if len(v.Checks) != len(criteria) {
		return fmt.Errorf("checks must contain exactly one entry for each acceptance criterion; expected %d, got %d", len(criteria), len(v.Checks))
	}
	valid := map[uint64]bool{}
	for _, criterion := range criteria {
		valid[criterion.ID] = true
	}
	seen := map[uint64]bool{}
	allVerified := true
	for _, check := range v.Checks {
		if !valid[check.CriterionID] {
			return fmt.Errorf("unknown criterion_id %d", check.CriterionID)
		}
		if seen[check.CriterionID] {
			return fmt.Errorf("duplicate criterion_id %d", check.CriterionID)
		}
		seen[check.CriterionID] = true
		switch check.Status {
		case state.AcceptanceVerified:
			if len(check.Evidence) == 0 {
				return fmt.Errorf("criterion %d marked verified without evidence", check.CriterionID)
			}
		case state.AcceptancePartial, state.AcceptanceUnverified:
			allVerified = false
			if strings.TrimSpace(check.Missing) == "" {
				return fmt.Errorf("criterion %d status %q requires missing explanation", check.CriterionID, check.Status)
			}
		default:
			return fmt.Errorf("invalid acceptance status %q; allowed values: verified, partial, unverified", check.Status)
		}
	}
	if v.Complete && !allVerified {
		return errors.New("complete=true requires every acceptance criterion to be verified")
	}
	if !v.Complete && allVerified {
		return errors.New("complete=false conflicts with all acceptance criteria being verified")
	}
	return nil
}

func (p *FinalTaskEvaluationPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in FinalTaskEvaluationInput) (FinalCheck, error) {
	prompt := FinalCheckPrompt
	if in.IsReadOnly {
		prompt = ReadOnlyFinalCheckPrompt
	}
	req := PromptRequest{
		PrimitiveInstructions: prompt,
		PrimitiveInput:        in.Purpose,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out FinalCheck
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelExecuting, p.baseSystem, user, &out, func() error {
		return ValidateFinalCheck(&out, in.Criteria)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("FinalTaskEvaluation", false)
		} else {
			pc.RecordInference("FinalTaskEvaluation", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "FinalTaskEvaluation", "final task evaluation", user, a.Raw)
	}

	return out, err
}
