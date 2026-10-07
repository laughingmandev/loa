package primitives

import (
	"context"
	"fmt"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

const criteriaDependencyPrompt = `Act as a software architect. Read these acceptance criteria.
If a Criterion Y relies on the structural or logical guarantees established by Criterion X, map X as a dependency of Y.
For example, if Criterion 1 is "Worker writes data sequentially" and Criterion 2 is "Database safely sends concurrent jobs to the worker", then Criterion 2 depends on 1 because validating the database's safety requires knowing the worker's guarantees.
Do not map circular dependencies.
Return a mapping array of ONLY the criteria that have dependencies: {"mappings":[{"criterion_id": 2, "depends_on": [1]}]}. If there are no dependencies, return {"mappings":[]}.`

type CriteriaDependencyMapping struct {
	CriterionID uint64   `json:"criterion_id"`
	DependsOn   []uint64 `json:"depends_on"`
}

type CriteriaDependencyResult struct {
	Mappings []CriteriaDependencyMapping `json:"mappings"`
}

type CriteriaDependencyInput struct {
	Purpose  string
	Criteria []state.AcceptanceCriterion
}

type CriteriaDependencyPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewCriteriaDependencyPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *CriteriaDependencyPrimitive {
	return &CriteriaDependencyPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *CriteriaDependencyPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in CriteriaDependencyInput) (CriteriaDependencyResult, error) {
	criteriaJSON := ""
	for _, c := range in.Criteria {
		criteriaJSON += fmt.Sprintf(`{"id": %d, "requirement": "%s"}`+"\n", c.ID, c.Requirement)
	}

	req := PromptRequest{
		PrimitiveInstructions: criteriaDependencyPrompt,
		PrimitiveInputLabel:   "\nCRITERIA TO EVALUATE:\n",
		PrimitiveInput:        criteriaJSON,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out CriteriaDependencyResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, user, &out, func() error {
		return validateCriteriaDependencies(&out, in.Criteria)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("DefineCriteriaDependencies", false)
		} else {
			pc.RecordInference("DefineCriteriaDependencies", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "DefineCriteriaDependencies", "map criteria dependencies", user, a.Raw)
	}

	return out, err
}

func validateCriteriaDependencies(v *CriteriaDependencyResult, original []state.AcceptanceCriterion) error {
	validIDs := make(map[uint64]bool)
	for _, c := range original {
		validIDs[c.ID] = true
	}

	for _, mapping := range v.Mappings {
		if !validIDs[mapping.CriterionID] {
			return fmt.Errorf("invalid criterion_id %d in mappings", mapping.CriterionID)
		}
		for _, dep := range mapping.DependsOn {
			if !validIDs[dep] {
				return fmt.Errorf("invalid dependency criterion_id %d", dep)
			}
			if dep == mapping.CriterionID {
				return fmt.Errorf("criterion %d cannot depend on itself", mapping.CriterionID)
			}
		}
	}
	return nil
}
