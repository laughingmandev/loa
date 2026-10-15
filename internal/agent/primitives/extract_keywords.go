package primitives

import (
	"context"

	"github.com/laughingmandev/loa/internal/config"
	"github.com/laughingmandev/loa/internal/llm"
	"github.com/laughingmandev/loa/internal/state"
)

var extractKeywordsPrompt = `Analyze the provided narrative frame or memory text. Extract a concise list of the most important structural and semantic keywords, concept names, identifiers, or file paths that represent its core meaning. These keys will be used for vector embedding and retrieval.
Return {"index_keys":["keyword1", "keyword2"]}.`

type KeywordExtraction struct {
	IndexKeys []string `json:"index_keys"`
}

type ExtractKeywordsInput struct {
	Text string
}

type ExtractKeywordsPrimitive struct {
	cfg        config.Config
	llm        llm.Client
	baseSystem string
}

func NewExtractKeywordsPrimitive(cfg config.Config, llmClient llm.Client, baseSystem string) *ExtractKeywordsPrimitive {
	return &ExtractKeywordsPrimitive{
		cfg:        cfg,
		llm:        llmClient,
		baseSystem: baseSystem,
	}
}

func (p *ExtractKeywordsPrimitive) Execute(ctx context.Context, pc PrimitiveContext, in ExtractKeywordsInput) (KeywordExtraction, error) {
	var ext KeywordExtraction
	req := PromptRequest{
		PrimitiveInstructions: extractKeywordsPrompt,
		PrimitiveInput:        in.Text,
		Step:                  nil,
		Extras:                nil,
		IncludeInstructions:   true,
		GlobalPlanView:        true,
	}
	var user string
	_ = user

	attempts, err := p.llm.ChatJSON(
		ctx,
		p.cfg.ModelCrawling,
		p.baseSystem,
		func(repairCost int) (string, error) {
		req.RepairReserve = repairCost
		prompt, promptErr := pc.BuildPrompt(req)
		if repairCost == 0 && promptErr == nil {
			user = prompt
		}
		return prompt, promptErr
	},
		&ext,
		nil, // no specific validation logic needed
	)

	for i := range attempts {
		if i == 0 {
			pc.RecordInference("ExtractKeywords", false)
		} else {
			pc.RecordInference("ExtractKeywords", true)
		}
	}

	for _, a := range attempts {
		kind := state.LogLLM
		if a.PromptKind == "repair" {
			kind = state.LogRepair
		}
		pc.Log(kind, "ExtractKeywords", "extract keywords", in.Text, a.Raw)
	}

	return ext, err
}
