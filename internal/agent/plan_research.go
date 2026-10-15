package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/state"
	"github.com/laughingmandev/loa/internal/tools"
)

// runPlanResearch runs the pre-planning research loop. Before acceptance criteria and the plan are
// written, the agent may read the chat history, attached files, memories and past task summaries.
// Results are appended to extras, which the caller passes on to criteria and planning.
// Reaching the configured loop cap is not an error: planning proceeds with what was collected.
func (e *Engine) runPlanResearch(ctx context.Context, goal string, extras []string) ([]string, error) {
	snap := e.store.Snapshot()
	// Frozen once per research run so read_history offsets stay stable while new messages arrive.
	historyMsgs := historyMessages(snap)

	allowedPaths := map[string]bool{}
	var allowedList []string
	for _, att := range snap.ActiveAttachments {
		p := filepath.ToSlash(filepath.Clean(att.VirtualPath))
		if !allowedPaths[p] {
			allowedPaths[p] = true
			allowedList = append(allowedList, p)
		}
	}

	purpose := "TASK GOAL:\n" + goal
	retrievalRound := 0
	maxLoops := e.cfg.Get().PlanResearchMaxLoops

	for loop := 0; loop < maxLoops; loop++ {
		if err := ctx.Err(); err != nil {
			return extras, err
		}
		prim := primitives.NewPlanResearchPrimitive(e.cfg.Get(), e.llm, baseSystem)
		d, err := prim.Execute(ctx, &engineContextBuilder{e: e}, primitives.PlanResearchInput{
			Purpose:        purpose,
			Extras:         extras,
			HasAttachments: len(allowedList) > 0,
		})
		if err != nil {
			return extras, err
		}

		switch d.Action {
		case "ready_to_plan":
			e.log(state.LogPlan, "PlanResearch", "ready to plan", d.Reason, "")
			return extras, nil
		case "retrieve_memory":
			cands, err := e.retrieveMemoryRound(ctx, d.Query, d.AllowedKinds, nil, retrievalRound)
			retrievalRound++
			if err != nil {
				extras = append(extras, "Memory retrieval error: "+err.Error())
			} else {
				extras = append(extras, "MEMORY RESULTS:\n"+cands)
			}
		case "task_summaries":
			extras = append(extras, e.taskSummaryText())
		case "read_history":
			extras = append(extras, e.readHistory(ctx, historyMsgs, d.Offset, d.Count, d.Reason))
		case "read_attachment":
			// PlanResearch exposes "read_attachment" instead of general "read_file" / "read_range" tool calls
			// so the model is not tempted to explore project source code before a plan exists.
			// Once validated against ActiveAttachments, we still route the read through e.executeTool and
			// formatToolResult so all standard safeguards apply: binary sniffing, output sanitization,
			// MaxStoredToolOutputBytes, MaxPromptToolOutputBytes artifact spill, and CodeBudget truncation.
			p := filepath.ToSlash(filepath.Clean(d.Path))
			if d.Path == "" || !allowedPaths[p] {
				allowed := "none"
				if len(allowedList) > 0 {
					allowed = strings.Join(allowedList, ", ")
				}
				extras = append(extras, fmt.Sprintf("Attachment read rejected: %q is not an active attachment. Attached files: %s", d.Path, allowed))
				continue
			}
			var toolReq tools.Request
			if d.Start > 0 || d.End > 0 {
				start := d.Start
				if start < 1 {
					start = 1
				}
				end := d.End
				if end < start {
					end = start + 120
				}
				rawInput, _ := json.Marshal(map[string]any{"path": d.Path, "start": start, "end": end})
				toolReq = tools.Request{Kind: state.ToolReadRange, Description: d.Reason, Input: rawInput}
			} else {
				rawInput, _ := json.Marshal(map[string]any{"path": d.Path})
				toolReq = tools.Request{Kind: state.ToolReadFile, Description: d.Reason, Input: rawInput}
			}
			res, err := e.executeTool(ctx, toolReq, false)
			if err != nil {
				return extras, err
			}
			extras = append(extras, formatToolResult(toolReq, res))
		}
	}

	e.log(state.LogPlan, "PlanResearch", "loop cap reached, planning with collected context", fmt.Sprintf("plan_research_max_loops=%d", maxLoops), "")
	return extras, nil
}
