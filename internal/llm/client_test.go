package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/laughingmandev/loa/internal/config"
)

func TestJSONAutoRepair(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		content := `{"answer":"ok"}`
		if calls == 1 {
			content = `not json`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer ts.Close()
	cfg := config.Default()
	cfg.OllamaURL = ts.URL
	cfg.ModelConversation = "test"
	cfg.JSONRepairAttempts = 3
	c := New(func() config.Config { return cfg })
	var out struct {
		Answer string `json:"answer"`
	}
	attempts, err := c.ChatJSON(context.Background(), "test", "sys", func(repairCost int) (string, error) { return "do it", nil }, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Answer != "ok" || len(attempts) != 2 {
		t.Fatalf("out=%+v attempts=%d", out, len(attempts))
	}
}

func TestJSONRepairZerosPartialStructBetweenAttempts(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		content := `{"a":"fresh","b":"present"}`
		switch calls {
		case 1:
			content = `{"a":"stale","b":"old"}`
		case 2:
			content = `{"a":"fresh"}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}})
	}))
	defer ts.Close()
	cfg := config.Default()
	cfg.OllamaURL = ts.URL
	cfg.ModelConversation = "test"
	cfg.JSONRepairAttempts = 4
	c := New(func() config.Config { return cfg })
	var out struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	attempts, err := c.ChatJSON(context.Background(), "test", "sys", func(repairCost int) (string, error) { return "do it", nil }, &out, func() error {
		if out.A != "fresh" || out.B != "present" {
			return fmt.Errorf("required fields not satisfied: a=%q b=%q", out.A, out.B)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(attempts))
	}
	if out.A != "fresh" || out.B != "present" {
		t.Fatalf("unexpected output: %+v", out)
	}
}
