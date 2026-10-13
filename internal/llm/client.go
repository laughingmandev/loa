package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/state"
)

type Message struct{ Role, Content string }

type Attempt struct {
	PromptKind string `json:"prompt_kind"`
	Prompt     string `json:"prompt"`
	Raw        string `json:"raw"`
	Error      string `json:"error,omitempty"`
}

type Client interface {
	Models(context.Context) ([]string, error)
	ChatJSON(ctx context.Context, model string, system string, buildPrompt func(repairCost int) (string, error), out any, validate func() error) ([]Attempt, error)
	ChatText(context.Context, string, string, string) (string, error)
	Embed(context.Context, string) ([]float32, error)
	Stats() state.RuntimeStats
}

type OpenAICompatible struct {
	Config func() config.Config
	HTTP   *http.Client
	inf    atomic.Uint64
	emb    atomic.Uint64
}

func New(cfg func() config.Config) *OpenAICompatible {
	return &OpenAICompatible{Config: cfg, HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

func (c *OpenAICompatible) Stats() state.RuntimeStats {
	return state.RuntimeStats{Inferences: c.inf.Load(), Embeddings: c.emb.Load()}
}

func (c *OpenAICompatible) Models(ctx context.Context) ([]string, error) {
	cfg := c.Config()
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, endpoint(cfg.OllamaURL, "/v1/models"), nil, &resp, time.Duration(cfg.LLMPollingTimeoutSeconds)*time.Second); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

func (c *OpenAICompatible) ChatText(ctx context.Context, model, system, user string) (string, error) {
	return c.chat(ctx, model, system, user, false)
}

func (c *OpenAICompatible) ChatJSON(ctx context.Context, model string, system string, buildPrompt func(repairCost int) (string, error), out any, validate func() error) ([]Attempt, error) {
	max := c.Config().JSONRepairAttempts
	if max <= 0 {
		max = 1
	}
	attempts := make([]Attempt, 0, max)
	kind := "original"

	user, err := buildPrompt(0)
	if err != nil {
		return nil, fmt.Errorf("failed to build initial prompt: %w", err)
	}
	prompt := user

	for i := 0; i < max; i++ {
		// NOTE: Ollama's json_object parser silently crashes (returns 0-length strings)
		// on large prompts with Qwen models. Default is disabled via ForceJSONMode.
		raw, err := c.chat(ctx, model, system, prompt, c.Config().ForceJSONMode)
		att := Attempt{PromptKind: kind, Prompt: prompt, Raw: raw}
		if err != nil {
			att.Error = err.Error()
			attempts = append(attempts, att)
			if strings.Contains(err.Error(), "model output was truncated") {
				prompt, err = repairPrompt(user, raw, att.Error, buildPrompt)
				if err != nil {
					return attempts, fmt.Errorf("failed to build repair prompt: %w", err)
				}
				kind = "repair"
				continue
			}
			return attempts, err
		}
		zeroOutput(out)

		// DEBUG PRINT START
		// trimLen := len(strings.TrimSpace(raw))
		// fmt.Printf("[DEBUG] ChatJSON decoding raw: len=%d, trimLen=%d, content=%s\n", len(raw), trimLen, raw)
		// DEBUG PRINT END

		if err := decodeStrict(raw, out); err == nil {
			if validate == nil {
				attempts = append(attempts, att)
				return attempts, nil
			}
			if verr := validate(); verr == nil {
				attempts = append(attempts, att)
				return attempts, nil
			} else {
				att.Error = verr.Error()
			}
		} else {
			att.Error = err.Error()
		}
		attempts = append(attempts, att)

		prompt, err = repairPrompt(user, raw, att.Error, buildPrompt)
		if err != nil {
			return attempts, fmt.Errorf("failed to build repair prompt: %w", err)
		}
		kind = "repair"
	}
	return attempts, fmt.Errorf("structured response invalid after %d attempts", max)
}

func zeroOutput(out any) {
	v := reflect.ValueOf(out)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v.Elem().Set(reflect.Zero(v.Elem().Type()))
	}
}

func decodeStrict(raw string, out any) error {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		raw = strings.TrimPrefix(raw, "```json")
		raw = strings.TrimPrefix(raw, "```")
		raw = strings.TrimSuffix(raw, "```")
		raw = strings.TrimSpace(raw)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("response contains trailing JSON/content")
	}
	return nil
}

func repairPrompt(originalUser, raw, parseErr string, buildPrompt func(int) (string, error)) (string, error) {
	if len(parseErr) > 2000 {
		parseErr = parseErr[:1950] + "\n... [TRUNCATED DUE TO ERROR SIZE]"
	}

	repairText := "Your previous response did not match the required JSON structure or validation rules.\n" +
		"Repair only what is needed to satisfy the original request and validation error. Return ONLY one JSON object, no markdown.\n\n" +
		"IMPORTANT REPAIR RULES:\n" +
		"- For enum/constrained fields, use ONLY the exact allowed values stated in the ORIGINAL REQUEST or VALIDATION ERROR.\n" +
		"- Do not invent synonyms, executable names, command names, aliases, or alternative enum labels.\n" +
		"- Preserve valid content from the previous response unless it must change to satisfy validation.\n" +
		"- If the VALIDATION ERROR gives a concrete structural correction (for example action=\"tool\" with a specific tool.kind), follow that correction literally while preserving the original semantic intent and tool input.\n" +
		"- If a field is optional and no valid value is appropriate, omit it or use the explicitly allowed empty value rather than inventing one.\n\n"

	overheadText := fmt.Sprintf("%s\nORIGINAL REQUEST:\n<OMITTED>\nPREVIOUS RESPONSE:\n%s\nVALIDATION ERROR:\n%s", repairText, raw, parseErr)
	repairCost := len(overheadText) / 4

	freshUser, err := buildPrompt(repairCost)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`%s

ORIGINAL REQUEST:
%s

PREVIOUS RESPONSE:
%s

VALIDATION ERROR:
%s`, repairText, freshUser, raw, parseErr), nil
}

func (c *OpenAICompatible) chat(ctx context.Context, model, system, user string, jsonMode bool) (string, error) {
	cfg := c.Config()
	if model == "" {
		return "", errors.New("no model selected for this inference")
	}
	body := map[string]any{
		"model":       model,
		"messages":    []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
		"stream":      false,
		"temperature": 0.2,
		"max_tokens":  cfg.OutputReserve,
	}
	if jsonMode {
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if !strings.Contains(cfg.OllamaURL, "api.openai.com") {
		if cfg.ContextBudget > 0 {
			body["num_ctx"] = cfg.NetworkContextBudget()
		}
		if cfg.OutputReserve > 0 {
			body["num_predict"] = cfg.OutputReserve
		}
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	c.inf.Add(1)

	// DEBUG PRINT START
	//bodyBytes, _ := json.Marshal(body)
	// fmt.Printf("[DEBUG] Sending LLM Request: System+User Length=%d, JSON Payload Size=%d bytes\n", len(system)+len(user), len(bodyBytes))
	// DEBUG PRINT END

	if err := c.do(ctx, http.MethodPost, endpoint(cfg.OllamaURL, "/v1/chat/completions"), body, &resp, time.Duration(cfg.ModelTimeoutSeconds)*time.Second); err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("model returned no choices")
	}
	if resp.Choices[0].FinishReason == "length" {
		return "", errors.New("model output was truncated (hit max_tokens or context limit)")
	}
	content := resp.Choices[0].Message.Content
	if len(strings.TrimSpace(content)) == 0 {
		return "", errors.New("model returned a 0-length response (Ollama KV cache corruption or silent crash)")
	}
	return content, nil
}

func (c *OpenAICompatible) Embed(ctx context.Context, input string) ([]float32, error) {
	cfg := c.Config()
	if cfg.EmbeddingModel == "" {
		return nil, errors.New("no embedding model selected")
	}
	body := map[string]any{"model": cfg.EmbeddingModel, "input": input}
	var resp struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	c.emb.Add(1)
	if err := c.do(ctx, http.MethodPost, endpoint(cfg.OllamaURL, "/v1/embeddings"), body, &resp, time.Duration(cfg.ModelTimeoutSeconds)*time.Second); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 || len(resp.Data[0].Embedding) == 0 {
		return nil, errors.New("embedding endpoint returned no vector")
	}
	return resp.Data[0].Embedding, nil
}

func (c *OpenAICompatible) do(ctx context.Context, method, url string, body any, out any, timeout time.Duration) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	cfg := c.Config()
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	client := *c.HTTP
	if timeout > 0 {
		client.Timeout = timeout
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ollama returned %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("decode ollama response: %w", err)
		}
	}
	return nil
}

func endpoint(base, path string) string { return strings.TrimRight(base, "/") + path }
