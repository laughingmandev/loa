package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/state"
)

func tailMessages(in []state.Message, n int) []state.Message {
	if n <= 0 || len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}
func tailSteps(in []state.StepResult, n int) []state.StepResult {
	if n <= 0 || len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}
func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func getPrerequisites(step *state.PlanStep, results []state.StepResult) []state.StepResult {
	if step == nil || len(step.DependsOn) == 0 {
		return nil
	}
	var prereqs []state.StepResult
	for _, depID := range step.DependsOn {
		for _, r := range results {
			if r.StepID == depID {
				prereqs = append(prereqs, r)
				break
			}
		}
	}
	return prereqs
}

func getBreadcrumbs(results []state.StepResult, n int) []string {
	tail := tailSteps(results, n)
	var crumbs []string
	for _, r := range tail {
		status := "Success"
		if !r.Success {
			status = "Failed"
		}
		crumbs = append(crumbs, fmt.Sprintf("[Step %d] %s: %s", r.StepID, status, r.Summary))
	}
	return crumbs
}

func buildPrompt(root string, cfg config.Config, snap state.AgentState, baseSystem string, req primitives.PromptRequest) (string, error) {
	var mandatory strings.Builder
	fmt.Fprintf(&mandatory, "CURRENT TIME: %s\nPROJECT ROOT: %s\n", time.Now().Format(time.RFC3339), root)
	if req.IncludeInstructions && strings.TrimSpace(cfg.ProjectInstructions) != "" {
		fmt.Fprintf(&mandatory, "\nPERSISTENT PROJECT INSTRUCTIONS:\n%s\n", cfg.ProjectInstructions)
	}
	if snap.PendingQuestion != nil {
		b, _ := json.MarshalIndent(snap.PendingQuestion, "", "  ")
		mandatory.WriteString("\nPENDING CLARIFICATION:\n")
		mandatory.Write(b)
		mandatory.WriteByte('\n')
	}

	if snap.ActiveTask != nil {
		mandatory.WriteString("\n## ACTIVE TASK OVERVIEW\n")
		fmt.Fprintf(&mandatory, "**ID:** %d | **Status:** %s | **May Modify:** %v\n", snap.ActiveTask.ID, snap.ActiveTask.Status, snap.ActiveTask.MayModify)
		fmt.Fprintf(&mandatory, "**Goal:** %s\n", snap.ActiveTask.Goal)
		if snap.ActiveTask.Target != "" {
			fmt.Fprintf(&mandatory, "**Target:** %s\n", snap.ActiveTask.Target)
		}
		if snap.ActiveTask.Failure != nil {
			mandatory.WriteString("\n**Failure State:**\n")
			b, _ := json.Marshal(snap.ActiveTask.Failure)
			mandatory.Write(b)
			mandatory.WriteByte('\n')
		}
		if len(snap.ActiveTask.Guidance) > 0 {
			mandatory.WriteString("\n**User Guidance:**\n")
			for _, g := range snap.ActiveTask.Guidance {
				fmt.Fprintf(&mandatory, "- [%s] %s\n", g.CreatedAt.Format(time.RFC3339), g.Text)
			}
		}

		if len(snap.ActiveTask.AcceptanceCriteria) > 0 || len(snap.ActiveTask.AcceptanceChecks) > 0 {
			mandatory.WriteString("\n## VERIFICATION\n")
			if len(snap.ActiveTask.AcceptanceCriteria) > 0 {
				mandatory.WriteString("### Acceptance Criteria\n")
				for _, ac := range snap.ActiveTask.AcceptanceCriteria {
					depStr := "none"
					if len(ac.DependsOn) > 0 {
						var deps []string
						for _, d := range ac.DependsOn {
							deps = append(deps, fmt.Sprintf("%d", d))
						}
						depStr = strings.Join(deps, ", ")
					}
					fmt.Fprintf(&mandatory, "- [ID: %d] %s (Depends on: %s)\n", ac.ID, ac.Requirement, depStr)
				}
			}
			if len(snap.ActiveTask.AcceptanceChecks) > 0 {
				mandatory.WriteString("### Acceptance Checks\n")
				for _, chk := range snap.ActiveTask.AcceptanceChecks {
					fmt.Fprintf(&mandatory, "- [Criterion %d] Status: %s\n", chk.CriterionID, chk.Status)
					if chk.Missing != "" {
						fmt.Fprintf(&mandatory, "  Missing: %s\n", chk.Missing)
					}
				}
			}
		}

		breadcrumbs := getBreadcrumbs(snap.ActiveTask.StepResults, cfg.ContextRecentBreadcrumbs)
		if len(breadcrumbs) > 0 {
			mandatory.WriteString("\n## RECENT TRAIL\n")
			for _, b := range breadcrumbs {
				fmt.Fprintf(&mandatory, "- %s\n", b)
			}
		}

		prereqs := getPrerequisites(req.Step, snap.ActiveTask.StepResults)
		if len(prereqs) > 0 {
			mandatory.WriteString("\n## PREREQUISITE RESULTS\n")
			for _, pr := range prereqs {
				status := "Success"
				if !pr.Success {
					status = "Failed"
				}
				fmt.Fprintf(&mandatory, "### [Step %d] %s\n", pr.StepID, status)
				fmt.Fprintf(&mandatory, "**Summary:** %s\n", pr.Summary)
			}
		}

		if len(snap.ActiveTask.Context.Facts) > 0 || len(snap.ActiveTask.Context.Decisions) > 0 {
			mandatory.WriteString("\n## WORKING MEMORY\n")
			if len(snap.ActiveTask.Context.Facts) > 0 {
				mandatory.WriteString("### Facts\n")
				for _, f := range snap.ActiveTask.Context.Facts {
					fmt.Fprintf(&mandatory, "- %s\n", f)
				}
			}
			if len(snap.ActiveTask.Context.Decisions) > 0 {
				mandatory.WriteString("### Decisions\n")
				for _, d := range snap.ActiveTask.Context.Decisions {
					fmt.Fprintf(&mandatory, "- %s\n", d)
				}
			}
		}

		if len(snap.ActiveTask.Plan.Steps) > 0 {
			mandatory.WriteString("\n## PLAN\n")

			// Contextual Plan View logic
			if req.GlobalPlanView {
				// Full plan for Global primitives
				b, _ := json.Marshal(snap.ActiveTask.Plan)
				mandatory.Write(b)
				mandatory.WriteByte('\n')
			} else {
				// Compressed 1-hop plan for Local primitives
				type shortStep struct {
					ID        uint64   `json:"id"`
					Status    string   `json:"status"`
					Goal      string   `json:"goal"`
					DependsOn []uint64 `json:"depends_on,omitempty"`
				}
				type compressedPlan struct {
					Version uint64 `json:"version"`
					Steps   []any  `json:"steps"`
				}

				var cPlan compressedPlan
				cPlan.Version = snap.ActiveTask.Plan.Version

				// Identify 1-hop relationships
				activeID := uint64(0)
				if req.Step != nil {
					activeID = req.Step.ID
				}

				var parentIDs []uint64
				if req.Step != nil {
					parentIDs = req.Step.DependsOn
				}

				isParent := func(id uint64) bool {
					for _, pid := range parentIDs {
						if pid == id {
							return true
						}
					}
					return false
				}

				for _, s := range snap.ActiveTask.Plan.Steps {
					// 1-hop if it's the current step, a direct parent, or a direct child
					isChild := false
					for _, dep := range s.DependsOn {
						if dep == activeID {
							isChild = true
							break
						}
					}

					if s.ID == activeID || isParent(s.ID) || isChild {
						cPlan.Steps = append(cPlan.Steps, s)
					} else {
						cPlan.Steps = append(cPlan.Steps, shortStep{
							ID:        s.ID,
							Status:    string(s.Status),
							Goal:      s.Goal,
							DependsOn: s.DependsOn,
						})
					}
				}

				b, _ := json.Marshal(cPlan)
				mandatory.Write(b)
				mandatory.WriteByte('\n')
			}
		}
	}

	if req.Step != nil {
		mandatory.WriteString("\n## CURRENT STEP\n")
		b, _ := json.Marshal(req.Step)
		mandatory.Write(b)
		mandatory.WriteByte('\n')
	}

	if len(snap.ActiveAttachments) > 0 {
		mandatory.WriteString("\nUSER ATTACHMENTS (Read-Only):\n")
		for _, att := range snap.ActiveAttachments {
			readOnly := false
			if att.Type == state.AttachmentTypeUpload {
				for _, up := range snap.Uploads {
					if up.ID == att.ID {
						readOnly = up.ReadOnly
						break
					}
				}
			} else {
				readOnly = true
			}
			if readOnly {
				fmt.Fprintf(&mandatory, "- [READ ONLY] %s\n", att.VirtualPath)
			} else {
				fmt.Fprintf(&mandatory, "- %s\n", att.VirtualPath)
			}
		}
	}

	if req.PinnedEvaluation != "" {
		mandatory.WriteString("\n=== PINNED EVALUATION (Your current goal) ===\n")
		mandatory.WriteString(req.PinnedEvaluation)
		mandatory.WriteString("\n=============================================\n")
	}
	if len(req.BlindTrail) > 0 {
		mandatory.WriteString("\n=== ACTIONS TAKEN SINCE LAST EVALUATION ===\n")
		for _, bt := range req.BlindTrail {
			mandatory.WriteString(bt)
			mandatory.WriteByte('\n')
		}
		mandatory.WriteString("===========================================\n")
	}

	// 1. Calculate length of Immutable Strings
	systemLen := len([]rune(baseSystem))
	instrLen := len([]rune(req.PrimitiveInstructions))

	inputLabel := req.PrimitiveInputLabel
	if inputLabel == "" {
		inputLabel = "\nPRIMITIVE INPUT:\n"
	}
	labelLen := len([]rune(inputLabel))
	inputLen := len([]rune(req.PrimitiveInput))

	mandatoryLen := len([]rune(mandatory.String()))

	// Format strings wrappers
	wrapperLen := len([]rune("\nPRIMITIVE INSTRUCTIONS:\n\n\n")) // The spacing between blocks

	// Calculate base immutable cost (without heuristic safety padding)
	immutableCost := systemLen + instrLen + labelLen + inputLen + mandatoryLen + wrapperLen + (cfg.OutputReserve * 4) + (req.RepairReserve * 4)

	// Max allowed runes dynamically inherits the configured safety margins
	maxRunes := cfg.PromptContextBudget() * 4
	slackRunes := float64(maxRunes - immutableCost)

	if slackRunes < 0 {
		return "", fmt.Errorf("immutable prompt cost (%d) exceeds context budget (%d)", immutableCost, maxRunes)
	}

	// Format Chat History requested
	historyCount := req.IncludeChatHistory
	if historyCount == 0 && !req.OmitChatHistory {
		historyCount = cfg.RecentMessages
	}

	var chatRunes int
	var chatLines []string
	if historyCount > 0 && !req.OmitChatHistory {
		recent := tailMessages(snap.Messages, historyCount)
		if len(recent) > 0 {
			chatRunes += len([]rune("\nRECENT CONVERSATION:\n"))
		}
		for _, m := range recent {
			text := m.Text
			if snap.ActiveTask != nil && text == snap.ActiveTask.Goal {
				text = "[CONTENT MOVED TO ACTIVE TASK GOAL AT TOP OF PROMPT]"
			}
			line := fmt.Sprintf("[%s] %s: %s", m.CreatedAt.Format(time.RFC3339), m.Source, text)
			chatLines = append(chatLines, line)
			chatRunes += len([]rune(line)) + 1 // +1 for newline
		}
	}

	// Measure Extras
	var extrasRunes int
	for _, ext := range req.Extras {
		extrasRunes += len([]rune(ext)) + 1 // +1 for newline
	}

	codeCap := float64(cfg.CodeBudget * 4)
	if codeCap <= 0 {
		codeCap = 32000
	}

	requestedChatRunes := float64(chatRunes)
	requestedExtraRunes := float64(extrasRunes)

	// Apply cap to extras if it exceeds CodeBudget
	if requestedExtraRunes > codeCap {
		requestedExtraRunes = codeCap
	}

	allocatedChat := requestedChatRunes
	allocatedExtra := requestedExtraRunes

	if requestedChatRunes+requestedExtraRunes > slackRunes {
		// Squash proportionally
		totalRequested := requestedChatRunes + requestedExtraRunes
		chatRatio := requestedChatRunes / totalRequested
		allocatedChat = slackRunes * chatRatio
		allocatedExtra = slackRunes - allocatedChat
	}

	// Assemble actual extras within allocatedExtra
	var finalExtras strings.Builder
	currentExtraRunes := 0
	for _, ext := range req.Extras {
		runes := []rune(ext)
		if currentExtraRunes+len(runes)+1 > int(allocatedExtra) {
			rem := int(allocatedExtra) - currentExtraRunes
			if rem > 10 {
				finalExtras.WriteString(string(runes[:rem-10]))
				finalExtras.WriteString("... (truncated)\n")
			}
			break
		}
		finalExtras.WriteString(ext)
		finalExtras.WriteByte('\n')
		currentExtraRunes += len(runes) + 1
	}

	// Assemble actual chat within allocatedChat
	var finalChat strings.Builder
	if len(chatLines) > 0 {
		finalChat.WriteString("\nRECENT CONVERSATION:\n")
		currentChatRunes := len([]rune("\nRECENT CONVERSATION:\n"))
		var reversedKept []string
		for i := len(chatLines) - 1; i >= 0; i-- {
			lineRunes := len([]rune(chatLines[i]))
			if currentChatRunes+lineRunes+1 > int(allocatedChat) {
				break
			}
			reversedKept = append(reversedKept, chatLines[i])
			currentChatRunes += lineRunes + 1
		}
		for i := len(reversedKept) - 1; i >= 0; i-- {
			finalChat.WriteString(reversedKept[i])
			finalChat.WriteByte('\n')
		}
	}

	// Final Assembly
	var fullPrompt strings.Builder
	fullPrompt.WriteString(mandatory.String())
	if finalExtras.Len() > 0 {
		fullPrompt.WriteString(finalExtras.String())
	}
	if finalChat.Len() > 0 {
		fullPrompt.WriteString(finalChat.String())
	}

	fullPrompt.WriteString("\nPRIMITIVE INSTRUCTIONS:\n")
	fullPrompt.WriteString(req.PrimitiveInstructions)
	fullPrompt.WriteString("\n")
	fullPrompt.WriteString(inputLabel)
	fullPrompt.WriteString(req.PrimitiveInput)

	return fullPrompt.String(), nil
}
