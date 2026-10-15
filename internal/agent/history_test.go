package agent

import (
	"testing"

	"github.com/laughingmandev/loa/internal/state"
)

func historyTestMessages(n int) []state.Message {
	msgs := make([]state.Message, n)
	for i := range msgs {
		msgs[i] = state.Message{ID: uint64(i + 1), Text: string(rune('a' + i))}
	}
	return msgs
}

func TestHistoryWindow(t *testing.T) {
	msgs := historyTestMessages(10) // texts a..j, j is the newest (offset 0)

	cases := []struct {
		name             string
		offset, count    int
		wantTexts        string // oldest-first
		wantFrom, wantTo int
	}{
		{"newest three", 0, 3, "hij", 0, 2},
		{"middle", 3, 4, "defg", 3, 6},
		{"count larger than available", 7, 10, "abc", 7, 9},
		{"exactly all", 0, 10, "abcdefghij", 0, 9},
		{"offset beyond total", 10, 3, "", 0, 0},
		{"negative offset", -1, 3, "", 0, 0},
		{"zero count", 0, 0, "", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			win, from, to, total := historyWindow(msgs, tc.offset, tc.count)
			got := ""
			for _, m := range win {
				got += m.Text
			}
			if got != tc.wantTexts {
				t.Fatalf("texts = %q, want %q", got, tc.wantTexts)
			}
			if total != 10 {
				t.Fatalf("total = %d, want 10", total)
			}
			if len(win) > 0 && (from != tc.wantFrom || to != tc.wantTo) {
				t.Fatalf("offsets = %d-%d, want %d-%d", from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestHistoryMessagesReplacesTriggeringGoalWithoutMutatingSource(t *testing.T) {
	orig := []state.Message{
		{ID: 1, Source: state.MessageUser, Text: "do task"},
		{ID: 2, Source: state.MessageAgent, Text: "done first"},
		{ID: 3, Source: state.MessageUser, Text: "do task"},
		{ID: 4, Source: state.MessageAgent, Text: "clarification question?"},
		{ID: 5, Source: state.MessageUser, Text: "my answer"},
	}
	snap := state.AgentState{
		Messages: orig,
		ActiveTask: &state.TaskState{
			Goal: "do task\n\nUSER FEEDBACK FOR REPLAN:\nadjust step 2",
		},
	}

	got := historyMessages(snap)
	if len(got) != len(orig) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(orig))
	}
	// Only the newest matching user message (ID 3, index 2) is replaced; older identical message (ID 1) and newer clarification (ID 5) stay intact.
	if got[0].Text != "do task" {
		t.Fatalf("got[0].Text = %q, want %q", got[0].Text, "do task")
	}
	if got[2].Text != movedToTaskObjectivePlaceholder {
		t.Fatalf("got[2].Text = %q, want %q", got[2].Text, movedToTaskObjectivePlaceholder)
	}
	if got[4].Text != "my answer" {
		t.Fatalf("got[4].Text = %q, want %q", got[4].Text, "my answer")
	}
	// Original slice must not be mutated.
	if orig[2].Text != "do task" {
		t.Fatalf("orig[2].Text was mutated to %q", orig[2].Text)
	}
}

