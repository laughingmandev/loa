package primitives

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

// ReplanStepMapping wires a newly replanned pending step to completed step IDs it depends on.
type ReplanStepMapping struct {
	StepID             uint64   `json:"step_id"`
	CompletedDependsOn []uint64 `json:"completed_depends_on"`
}

type LinkReplanResult struct {
	Mappings           []ReplanStepMapping `json:"mappings,omitempty"`
	Reason             string              `json:"reason,omitempty"`
	Action             string              `json:"action,omitempty"`
	ReplanInstructions string              `json:"replan_instructions,omitempty"`
}

type LinkReplanInput struct {
	CompletedSteps []state.PlanStep
	StepResults    []state.StepResult
	NewSteps       []state.PlanStep
}

type LinkReplanPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewLinkReplanPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *LinkReplanPrimitive {
	return &LinkReplanPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var LinkReplanPrompt = `You are wiring newly replanned pending steps into the existing graph of already-completed steps.
When a plan is replanned mid-task, the newly drafted steps already have their internal dependencies (between new steps) assigned, but they are not yet connected to the completed steps.
In Loa, a pending step ONLY receives the execution summary (PREREQUISITE RESULTS) of a completed step if that completed step's ID is listed in the pending step's dependencies.
Note: Ignore any old "pending" steps shown in the ## PLAN section above — those are being replaced by this replan. Only use the step IDs listed under COMPLETED STEPS and NEWLY REPLANNED PENDING STEPS in PRIMITIVE INPUT below.

RULES:
1. Connect the Graph (Mandatory for Root New Steps): Every new step with "requires_completed_link": true (i.e. empty "internal_depends_on") MUST be linked to at least one completed step ID in "completed_depends_on" so the new steps do not form a disconnected tree.
2. Feed Prerequisite Information Forward: Link new steps (especially root steps, or later new steps that directly build on prior findings) to the specific completed steps whose findings/outputs they require as input. Pay special attention to completed steps marked "is_leaf_of_completed_graph": true, as these represent the latest unconsumed outputs of each completed branch, though you may link to any relevant completed step ID.
3. Max Prerequisites Rule: A single step MAY NOT depend on more than %d previous steps in total (len(internal_depends_on) + len(completed_depends_on) <= %d). Never exceed "remaining_dependency_slots" for any new step.
4. Direction of Wiring: Completed steps are immutable history. You are adding completed step IDs as prerequisites ("completed_depends_on") onto NEW pending steps ("step_id"). Never put a completed step ID in "step_id", and never put a new pending step ID in "completed_depends_on".

Return format:
{"mappings":[{"step_id": 2610, "completed_depends_on": [2311, 2312]}], "reason": "short explanation"}

ESCALATION (REPLAN):
If the newly drafted steps cannot be wired to the required completed steps without exceeding the %d prerequisites limit per step (for example, if more intermediate synthesis steps are needed to gather completed branches), escalate by returning:
{"action":"replan", "reason":"...", "replan_instructions":"Clear explanation of how the replanned steps must be structured (e.g. adding intermediate synthesis steps) to respect the %d prerequisites limit."}`

type completedStepLinkView struct {
	ID                      uint64   `json:"id"`
	Title                   string   `json:"title"`
	Goal                    string   `json:"goal"`
	DependsOn               []uint64 `json:"depends_on,omitempty"`
	Summary                 string   `json:"summary,omitempty"`
	IsLeafOfCompletedSubDAG bool     `json:"is_leaf_of_completed_graph"`
}

type newStepLinkView struct {
	ID                       uint64         `json:"id"`
	Title                    string         `json:"title"`
	Goal                     string         `json:"goal"`
	Reason                   string         `json:"reason"`
	Mode                     state.StepMode `json:"mode"`
	InternalDependsOn        []uint64       `json:"internal_depends_on,omitempty"`
	RequiresCompletedLink    bool           `json:"requires_completed_link"`
	RemainingDependencySlots int            `json:"remaining_dependency_slots"`
}

func (p *LinkReplanPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in LinkReplanInput) (LinkReplanResult, error) {
	summaryByStep := make(map[uint64]string, len(in.StepResults))
	for _, sr := range in.StepResults {
		if sr.Summary != "" {
			summaryByStep[sr.StepID] = sr.Summary
		}
	}

	consumedByCompleted := make(map[uint64]bool, len(in.CompletedSteps))
	for _, cs := range in.CompletedSteps {
		for _, dep := range cs.DependsOn {
			consumedByCompleted[dep] = true
		}
	}

	completedViews := make([]completedStepLinkView, 0, len(in.CompletedSteps))
	for _, cs := range in.CompletedSteps {
		completedViews = append(completedViews, completedStepLinkView{
			ID:                      cs.ID,
			Title:                   cs.Title,
			Goal:                    cs.Goal,
			DependsOn:               cs.DependsOn,
			Summary:                 summaryByStep[cs.ID],
			IsLeafOfCompletedSubDAG: !consumedByCompleted[cs.ID],
		})
	}

	maxPrereqs := p.cfg.MaxPrerequisitesPerStep
	newViews := make([]newStepLinkView, 0, len(in.NewSteps))
	for _, ns := range in.NewSteps {
		remaining := maxPrereqs - len(ns.DependsOn)
		if remaining < 0 {
			remaining = 0
		}
		newViews = append(newViews, newStepLinkView{
			ID:                       ns.ID,
			Title:                    ns.Title,
			Goal:                     ns.Goal,
			Reason:                   ns.Reason,
			Mode:                     ns.Mode,
			InternalDependsOn:        ns.DependsOn,
			RequiresCompletedLink:    len(ns.DependsOn) == 0,
			RemainingDependencySlots: remaining,
		})
	}

	compBytes, _ := json.Marshal(completedViews)
	newBytes, _ := json.Marshal(newViews)
	purpose := fmt.Sprintf("COMPLETED STEPS (Available Prerequisite Inputs):\n%s\n\nNEWLY REPLANNED PENDING STEPS (Receivers to Wire):\n%s", string(compBytes), string(newBytes))

	req := PromptRequest{
		PrimitiveInstructions: fmt.Sprintf(LinkReplanPrompt, maxPrereqs, maxPrereqs, maxPrereqs, maxPrereqs),
		PrimitiveInput:        purpose,
		IncludeInstructions:   true,
		GlobalPlanView:        false,
	}
	var user string

	var out LinkReplanResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelPlanning, p.baseSystem, func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	}, &out, func() error {
		return ValidateLinkReplan(&out, in.CompletedSteps, in.NewSteps, maxPrereqs)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("LinkReplan", false)
		} else {
			pc.RecordInference("LinkReplan", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "LinkReplan", "link replanned steps to completed steps", user, a.Raw)
	}

	return out, err
}

func ValidateLinkReplan(out *LinkReplanResult, completed []state.PlanStep, newSteps []state.PlanStep, maxPrerequisites int) error {
	if out.Action == "replan" {
		if strings.TrimSpace(out.Reason) == "" && strings.TrimSpace(out.ReplanInstructions) == "" {
			return fmt.Errorf("Validation Error: action=\"replan\" requires non-empty reason and replan_instructions.")
		}
		return nil
	}
	if out.Action != "" {
		return fmt.Errorf("Validation Error: invalid action %q; omit action when returning mappings, or use action=\"replan\" to escalate.", out.Action)
	}
	if err := NeedReason(out.Reason); err != nil {
		return err
	}

	completedSet := make(map[uint64]bool, len(completed))
	for _, cs := range completed {
		completedSet[cs.ID] = true
	}

	newStepMap := make(map[uint64]state.PlanStep, len(newSteps))
	for _, ns := range newSteps {
		newStepMap[ns.ID] = ns
	}

	mergedDeps := make(map[uint64][]uint64, len(newSteps))
	seenPerStep := make(map[uint64]map[uint64]bool, len(newSteps))

	for _, m := range out.Mappings {
		ns, ok := newStepMap[m.StepID]
		if !ok {
			if completedSet[m.StepID] {
				return fmt.Errorf("Validation Error: step_id %d is an already-completed step. step_id must be a NEW pending step ID, and completed_depends_on must list the completed step IDs it depends on.", m.StepID)
			}
			return fmt.Errorf("Validation Error: unknown new step_id %d in mappings.", m.StepID)
		}
		if seenPerStep[ns.ID] == nil {
			seenPerStep[ns.ID] = make(map[uint64]bool)
			for _, existing := range ns.DependsOn {
				seenPerStep[ns.ID][existing] = true
			}
		}
		for _, depID := range m.CompletedDependsOn {
			if !completedSet[depID] {
				if _, isNew := newStepMap[depID]; isNew {
					return fmt.Errorf("Validation Error: step %d in completed_depends_on for step %d is a new pending step, not a completed step. Only include completed step IDs in completed_depends_on.", depID, m.StepID)
				}
				return fmt.Errorf("Validation Error: unknown completed step ID %d in completed_depends_on for step %d.", depID, m.StepID)
			}
			if !seenPerStep[ns.ID][depID] {
				seenPerStep[ns.ID][depID] = true
				mergedDeps[ns.ID] = append(mergedDeps[ns.ID], depID)
			}
		}
	}

	for _, ns := range newSteps {
		compDeps := mergedDeps[ns.ID]
		if len(completed) > 0 && len(ns.DependsOn) == 0 && len(compDeps) == 0 {
			return fmt.Errorf("Validation Error: New root step %d (%q) has no internal dependencies and was not linked to any completed step. Every new step with requires_completed_link=true must be linked to at least one completed step so the plan forms a single connected graph.", ns.ID, ns.Title)
		}
		totalDeps := len(ns.DependsOn) + len(compDeps)
		if maxPrerequisites > 0 && totalDeps > maxPrerequisites {
			remaining := maxPrerequisites - len(ns.DependsOn)
			if remaining < 0 {
				remaining = 0
			}
			return fmt.Errorf("Validation Error: New step %d (%q) has %d internal dependencies and %d completed dependencies (%d total), which exceeds the maximum allowed %d prerequisites per step (remaining_dependency_slots=%d). Reduce completed_depends_on or return {\"action\":\"replan\",...} to introduce intermediate synthesis steps.", ns.ID, ns.Title, len(ns.DependsOn), len(compDeps), totalDeps, maxPrerequisites, remaining)
		}
	}

	return nil
}

// ApplyLinkReplanMappings prepends deduplicated completed step dependencies onto each mapped new step.
func ApplyLinkReplanMappings(newSteps []state.PlanStep, mappings []ReplanStepMapping) []state.PlanStep {
	out := make([]state.PlanStep, len(newSteps))
	copy(out, newSteps)

	byStep := make(map[uint64][]uint64, len(mappings))
	for _, m := range mappings {
		byStep[m.StepID] = append(byStep[m.StepID], m.CompletedDependsOn...)
	}

	for i := range out {
		compDeps := byStep[out[i].ID]
		if len(compDeps) == 0 {
			continue
		}
		seen := make(map[uint64]bool, len(compDeps)+len(out[i].DependsOn))
		merged := make([]uint64, 0, len(compDeps)+len(out[i].DependsOn))
		for _, d := range compDeps {
			if !seen[d] {
				seen[d] = true
				merged = append(merged, d)
			}
		}
		for _, d := range out[i].DependsOn {
			if !seen[d] {
				seen[d] = true
				merged = append(merged, d)
			}
		}
		out[i].DependsOn = merged
	}

	return out
}
