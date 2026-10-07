package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/laughingmandev/loa/internal/state"
)

type PermissionMode string

const (
	PermissionAskAll   PermissionMode = "ask_all"
	PermissionSelected PermissionMode = "ask_selected"
	PermissionAllowAll PermissionMode = "allow_all"
)

type PermissionConfig struct {
	Mode   PermissionMode          `json:"mode"`
	AskFor map[state.ToolKind]bool `json:"ask_for"`
}

type Config struct {
	OllamaURL             string           `json:"ollama_url"`
	APIKey                string           `json:"api_key"`
	ModelCrawling         string           `json:"model_crawling"`
	ModelConversation     string           `json:"model_conversation"`
	ModelPlanning         string           `json:"model_planning"`
	ModelExecuting        string           `json:"model_executing"`
	EmbeddingEngine       string           `json:"embedding_engine"`
	EmbeddingModel        string           `json:"embedding_model"`
	LocalEmbeddingModelPath string         `json:"local_embedding_model_path"`
	ContextBudget         int              `json:"context_budget"`
	OutputReserve         int              `json:"output_reserve"`
	CodeBudget            int              `json:"code_budget"`
	RecentMessages        int              `json:"recent_messages"`
	MemoryTopK            int              `json:"memory_top_k"`
	MemoryCandidatePool   int              `json:"memory_candidate_pool"`
	JSONRepairAttempts    int              `json:"json_repair_attempts"`
	MaxPlanDepth          int              `json:"max_plan_depth"`
	MaxExecutionLoops     int              `json:"max_execution_loops"`
	ModelTimeoutSeconds   int              `json:"model_timeout_seconds"`
	CommandTimeoutSeconds          int              `json:"command_timeout_seconds"`
	MemoryLTSDecayHalfLifeHours    float64          `json:"memory_lts_decay_half_life_hours"`
	MemoryLTSMaxWeight             float64          `json:"memory_lts_max_weight"`
	MemorySessionDecayHalfLifeHours float64         `json:"memory_session_decay_half_life_hours"`
	MemorySessionMaxWeight         float64          `json:"memory_session_max_weight"`
	ContextRecentBreadcrumbs       int              `json:"context_recent_breadcrumbs"`
	ContextMinimumBuffer           int              `json:"context_minimum_buffer"`
	ContextMinimumSafeBudget       int              `json:"context_minimum_safe_budget"`
	ContextCodeBudgetMaxFloor      int              `json:"context_code_budget_max_floor"`
	CrawlerSmallFileThreshold      int              `json:"crawler_small_file_threshold"`
	CrawlerMaxChunkLines           int              `json:"crawler_max_chunk_lines"`
	MemoryRerankGates              int              `json:"memory_rerank_gates"`
	MemoryPoolExpansionLimit       int              `json:"memory_pool_expansion_limit"`
	FSWatcherDebounceMs            int              `json:"fs_watcher_debounce_ms"`
	MaxStoredToolOutputBytes       int              `json:"max_stored_tool_output_bytes"`
	MaxPromptToolOutputBytes       int              `json:"max_prompt_tool_output_bytes"`
	LLMPollingTimeoutSeconds       int              `json:"llm_polling_timeout_seconds"`
	ServerReadHeaderTimeoutSeconds int              `json:"server_read_header_timeout_seconds"`
	ProjectInstructions            string           `json:"project_instructions"`
	Permissions                    PermissionConfig `json:"permissions"`
	BlindActionThreshold           int              `json:"blind_action_threshold"`
	EvaluationMode                 string           `json:"evaluation_mode"`
	MaxPrerequisitesPerStep        int              `json:"max_prerequisites_per_step"`
}

func Default() Config {
	home, _ := os.UserHomeDir()
	defaultLocalEmbed := filepath.Join(home, ".loa", "embedding-models", "nomic-embed-text-v1.5.f16.gguf")

	return Config{
		OllamaURL:             "http://127.0.0.1:11434",
		EmbeddingEngine:       "local",
		LocalEmbeddingModelPath: defaultLocalEmbed,
		ContextBudget:         100000,
		OutputReserve:         4000,
		CodeBudget:            8000,
		RecentMessages:        12,
		MemoryTopK:            8,
		MemoryCandidatePool:   30,
		JSONRepairAttempts:    7,
		MaxPlanDepth:          25,
		MaxExecutionLoops:     80,
		ModelTimeoutSeconds:   600,
		CommandTimeoutSeconds:          120,
		MemoryLTSDecayHalfLifeHours:    336.0,
		MemoryLTSMaxWeight:             0.03,
		MemorySessionDecayHalfLifeHours: 2.0,
		MemorySessionMaxWeight:         0.20,
		ContextRecentBreadcrumbs:       5,
		ContextMinimumBuffer:           2000,
		ContextMinimumSafeBudget:       4000,
		ContextCodeBudgetMaxFloor:      8000,
		CrawlerSmallFileThreshold:      200,
		CrawlerMaxChunkLines:           150,
		MemoryRerankGates:              3,
		MemoryPoolExpansionLimit:       4,
		FSWatcherDebounceMs:            500,
		MaxStoredToolOutputBytes:       4194304,
		MaxPromptToolOutputBytes:       30000,
		LLMPollingTimeoutSeconds:       15,
		ServerReadHeaderTimeoutSeconds: 10,
		BlindActionThreshold:           8,
		EvaluationMode:                 "Dynamic",
		MaxPrerequisitesPerStep:        4,
		Permissions: PermissionConfig{
			Mode: PermissionSelected,
			AskFor: map[state.ToolKind]bool{
				state.ToolWriteFile:      true,
				state.ToolPatchFile:      true,
				state.ToolDeleteFile:     true,
				state.ToolDeleteDir:      true,
				state.ToolExecuteProcess: true,
				state.ToolExecuteShell:   true,
			},
		},
	}
}

func GlobalPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "loa", "config.json"), nil
}

func ProjectPath(root string) string { return filepath.Join(root, ".loa.json") }

func Load(root string) (Config, error) {
	cfg := Default()
	if p, err := GlobalPath(); err == nil {
		if err := mergeFile(&cfg, p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("load global config: %w", err)
		}
	}
	if err := mergeFile(&cfg, ProjectPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, fmt.Errorf("load project config: %w", err)
	}
	cfg.normalize()
	return cfg, nil
}

func mergeFile(cfg *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, cfg)
}

func SaveGlobal(cfg Config) error {
	p, err := GlobalPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return atomicJSON(p, cfg, 0o600)
}

func SaveProject(root string, cfg Config) error {
	return atomicJSON(ProjectPath(root), cfg, 0o600)
}

func atomicJSON(path string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".loa-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (c *Config) normalize() {
	if c.OllamaURL == "" {
		c.OllamaURL = "http://127.0.0.1:11434"
	}
	if c.ContextBudget <= 0 {
		c.ContextBudget = 100000
	}
	if c.OutputReserve <= 0 {
		c.OutputReserve = 4000
	}
	if c.CodeBudget <= 0 {
		c.CodeBudget = 8000
	}
	if c.RecentMessages <= 0 {
		c.RecentMessages = 12
	}
	if c.MemoryTopK <= 0 {
		c.MemoryTopK = 8
	}
	if c.MemoryCandidatePool <= 0 {
		c.MemoryCandidatePool = 30
	}
	if c.JSONRepairAttempts <= 0 {
		c.JSONRepairAttempts = 7
	}
	if c.MaxPlanDepth <= 0 {
		c.MaxPlanDepth = 25
	}
	if c.ModelTimeoutSeconds <= 0 {
		c.ModelTimeoutSeconds = 600
	}
	if c.CommandTimeoutSeconds <= 0 {
		c.CommandTimeoutSeconds = 120
	}
	if c.Permissions.AskFor == nil {
		c.Permissions.AskFor = map[state.ToolKind]bool{}
	}
	if c.Permissions.Mode == "" {
		c.Permissions.Mode = PermissionSelected
	}
	if c.MemoryLTSDecayHalfLifeHours <= 0 {
		c.MemoryLTSDecayHalfLifeHours = 336.0
	}
	if c.MemoryLTSMaxWeight <= 0 {
		c.MemoryLTSMaxWeight = 0.03
	}
	if c.MemorySessionDecayHalfLifeHours <= 0 {
		c.MemorySessionDecayHalfLifeHours = 2.0
	}
	if c.MemorySessionMaxWeight <= 0 {
		c.MemorySessionMaxWeight = 0.20
	}
	if c.ContextRecentBreadcrumbs <= 0 {
		c.ContextRecentBreadcrumbs = 5
	}
	if c.ContextMinimumBuffer <= 0 {
		c.ContextMinimumBuffer = 2000
	}
	if c.ContextMinimumSafeBudget <= 0 {
		c.ContextMinimumSafeBudget = 4000
	}
	if c.ContextCodeBudgetMaxFloor <= 0 {
		c.ContextCodeBudgetMaxFloor = 8000
	}
	if c.CrawlerSmallFileThreshold <= 0 {
		c.CrawlerSmallFileThreshold = 200
	}
	if c.CrawlerMaxChunkLines <= 0 {
		c.CrawlerMaxChunkLines = 150
	}
	if c.MemoryRerankGates <= 0 {
		c.MemoryRerankGates = 3
	}
	if c.MemoryPoolExpansionLimit <= 0 {
		c.MemoryPoolExpansionLimit = 4
	}
	if c.FSWatcherDebounceMs <= 0 {
		c.FSWatcherDebounceMs = 500
	}
	if c.MaxStoredToolOutputBytes <= 0 {
		c.MaxStoredToolOutputBytes = 4194304
	}
	if c.LLMPollingTimeoutSeconds <= 0 {
		c.LLMPollingTimeoutSeconds = 15
	}
	if c.ServerReadHeaderTimeoutSeconds <= 0 {
		c.ServerReadHeaderTimeoutSeconds = 10
	}
}
