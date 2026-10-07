package primitives

import (
	"context"
	"errors"
	"strings"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

type SummaryResult struct {
	Summary string `json:"summary"`
}

type SummarizeTaskInput struct {
	Purpose string
}

type SummarizeTaskPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewSummarizeTaskPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *SummarizeTaskPrimitive {
	return &SummarizeTaskPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

var SummarizeTaskPrompt = `Summarize the completed task in 1-3 sentences for a future agent asking "what have I already done?" Include important implementation locations/decisions and verified user-visible outcomes when known. Do not upgrade partial or mocked evidence into stronger verification than the supplied acceptance checks support. Return {"summary":"..."}.`

func ValidateSummary(v *SummaryResult) error {
	if strings.TrimSpace(v.Summary) == "" {
		return errors.New("summary required")
	}
	return nil
}

func (p *SummarizeTaskPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in SummarizeTaskInput) (SummaryResult, error) {
	req := PromptRequest{
		PrimitiveInstructions: SummarizeTaskPrompt,
		PrimitiveInput:        in.Purpose,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	user := pc.BuildPrompt(req)

	var out SummaryResult
	attempts, err := p.llm.ChatJSON(ctx, p.cfg.ModelCrawling, p.baseSystem, user, &out, func() error {
		return ValidateSummary(&out)
	})

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("SummarizeTask", false)
		} else {
			pc.RecordInference("SummarizeTask", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "SummarizeTask", "summarize task", user, a.Raw)
	}

	return out, err
}
