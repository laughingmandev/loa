package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/laughingmandev/loa/internal/agent/primitives"
	"github.com/laughingmandev/loa/internal/state"
)

const movedToTaskObjectivePlaceholder = "[THIS MESSAGE WAS MOVED TO TASK OBJECTIVE]"

// historyMessages returns a copy of snap.Messages where the most recent user message that triggered
// the active task has its Text replaced with movedToTaskObjectivePlaceholder. It allocates a new slice
// so the store and UI chat state are never mutated.
func historyMessages(snap state.AgentState) []state.Message {
	out := make([]state.Message, len(snap.Messages))
	copy(out, snap.Messages)
	if snap.ActiveTask == nil || strings.TrimSpace(snap.ActiveTask.Goal) == "" {
		return out
	}
	goal := snap.ActiveTask.Goal
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Source != state.MessageUser {
			continue
		}
		if out[i].Text == goal || strings.HasPrefix(goal, out[i].Text+"\n\nUSER FEEDBACK FOR REPLAN:\n") {
			out[i].Text = movedToTaskObjectivePlaceholder
			break
		}
	}
	return out
}

// historyWindow selects up to count messages starting at offset, where offset 0 is the newest message.
// The returned window is ordered oldest-first. fromOffset/toOffset describe the window in the same
// offset scale (toOffset >= fromOffset; toOffset is the oldest message included). If the offset is
// beyond the available history, the window is empty.
func historyWindow(msgs []state.Message, offset, count int) (window []state.Message, fromOffset, toOffset, total int) {
	total = len(msgs)
	if offset < 0 || count <= 0 || offset >= total {
		return nil, offset, offset, total
	}
	end := total - offset
	start := end - count
	if start < 0 {
		start = 0
	}
	window = msgs[start:end]
	return window, offset, offset + len(window) - 1, total
}

// readHistory fetches a range of chat messages and consolidates it against the agent's reason.
// msgs must be a snapshot taken once per loop so offsets stay stable while new messages arrive.
// The returned string is meant to be appended to the caller's extras.
func (e *Engine) readHistory(ctx context.Context, msgs []state.Message, offset, count int, reason string) string {
	limit := e.cfg.Get().RecentMessages
	if count <= 0 || count > limit {
		count = limit
	}

	window, from, to, total := historyWindow(msgs, offset, count)
	if len(window) == 0 {
		return fmt.Sprintf("CHAT HISTORY: no messages at offset %d (total %d, 0 = newest).", offset, total)
	}
	if len(window) == 1 && window[0].Text == movedToTaskObjectivePlaceholder {
		return fmt.Sprintf("CHAT HISTORY (offsets %d-%d of %d, 0 = newest): no prior messages in this range (only the current task objective).", from, to, total)
	}

	rangeLabel := fmt.Sprintf("CHAT MESSAGES (offsets %d-%d of %d, 0 = newest, listed oldest first)", from, to, total)
	prim := primitives.NewConsolidateHistoryPrimitive(e.cfg.Get(), e.llm, baseSystem)
	res, err := prim.Execute(ctx, &engineContextBuilder{e: e}, primitives.ConsolidateHistoryInput{
		Reason:     reason,
		RangeLabel: rangeLabel,
		Transcript: formatMessageLines(window),
	})
	if err != nil {
		return "Chat history read error: " + err.Error()
	}
	return fmt.Sprintf("CHAT HISTORY (offsets %d-%d of %d, 0 = newest) - reason: %s\n%s", from, to, total, reason, res.Extract)
}
