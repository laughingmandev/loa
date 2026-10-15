package state

import "time"

type Intent string

const (
	IntentStatement  Intent = "statement"
	IntentCorrection Intent = "correction"
	IntentQuestion   Intent = "question"
	IntentDiscussion Intent = "discussion"
	IntentTask       Intent = "task"
)

type IntentResult struct {
	Intents                []Intent `json:"intents"`
	TaskTitle              string   `json:"task_title,omitempty"`
	AnswersPendingQuestion bool     `json:"answers_pending_question"`

	Ambiguous              bool     `json:"ambiguous"`
	Reason                 string   `json:"reason"`
}

type MessageSource string

const (
	MessageUser  MessageSource = "user"
	MessageAgent MessageSource = "agent"
	MessageTool  MessageSource = "tool"
)

type Message struct {
	ID            uint64        `json:"id"`
	Source        MessageSource `json:"source"`
	Text          string        `json:"text"`
	CreatedAt     time.Time     `json:"created_at"`
	Intents       []Intent      `json:"intents,omitempty"`
	Clarification bool          `json:"clarification,omitempty"`
	Intervention  bool          `json:"intervention,omitempty"`
}

type TaskStatus string

const (
	TaskPlanning         TaskStatus = "planning"
	TaskAwaitingApproval TaskStatus = "awaiting_approval"
	TaskRunning          TaskStatus = "running"
	TaskPaused           TaskStatus = "paused"
	TaskWaiting          TaskStatus = "waiting_for_user"
	TaskCompleted        TaskStatus = "completed"
	TaskFailed           TaskStatus = "failed"
	TaskCancelled        TaskStatus = "cancelled"
)

type TaskContext struct {
	Facts       []string `json:"facts"`
	Decisions   []string `json:"decisions"`
	Constraints []string `json:"constraints"`
	OpenIssues  []string `json:"open_issues"`
	Artifacts   []string `json:"artifacts,omitempty"`
}

type AcceptanceStatus string

const (
	AcceptanceUnverified AcceptanceStatus = "unverified"
	AcceptancePartial    AcceptanceStatus = "partial"
	AcceptanceVerified   AcceptanceStatus = "verified"
)

type AcceptanceCriterion struct {
	ID          uint64   `json:"id"`
	Requirement string   `json:"requirement"`
	DependsOn   []uint64 `json:"depends_on,omitempty"`
}

type AcceptanceCheck struct {
	CriterionID uint64           `json:"criterion_id"`
	Status      AcceptanceStatus `json:"status"`
	Evidence    []string         `json:"evidence,omitempty"`
	Missing     string           `json:"missing,omitempty"`
}

type TaskFailure struct {
	FailedStepID         *uint64   `json:"failed_step_id,omitempty"`
	LastSuccessfulStepID *uint64   `json:"last_successful_step_id,omitempty"`
	Reason               string    `json:"reason"`
	Recoverable          bool      `json:"recoverable"`
	FailedAt             time.Time `json:"failed_at"`
}

type PlanStepStatus string

const (
	StepPending   PlanStepStatus = "pending"
	StepRunning   PlanStepStatus = "running"
	StepCompleted PlanStepStatus = "completed"
	StepFailed    PlanStepStatus = "failed"
	StepSkipped   PlanStepStatus = "skipped"
)

type StepMode string

const (
	StepInvestigate StepMode = "investigate"
	StepModify      StepMode = "modify"
	StepVerify      StepMode = "verify"
	StepSynthesize  StepMode = "synthesize"
)

type PlanStep struct {
	ID                 uint64         `json:"id"`
	Title              string         `json:"title"`
	Goal               string         `json:"goal"`
	Reason             string         `json:"reason"`
	ExpectedResult     string         `json:"expected_result"`
	Mode               StepMode       `json:"mode"`
	DependsOn          []uint64       `json:"depends_on,omitempty"`
	ExpectedTools      []ToolKind     `json:"expected_tools,omitempty"`
	Status             PlanStepStatus `json:"status"`
	DecompositionDepth int            `json:"decomposition_depth"`
	TargetEntities     []string       `json:"target_entities,omitempty"`
}

type Plan struct {
	Version uint64     `json:"version"`
	Steps   []PlanStep `json:"steps"`
}

type StepResult struct {
	StepID       uint64    `json:"step_id"`
	Success      bool      `json:"success"`
	Summary      string    `json:"summary"`
	FactsLearned []string  `json:"facts_learned,omitempty"`
	Decisions    []string  `json:"decisions,omitempty"`
	FilesRead    []string  `json:"files_read,omitempty"`
	FilesChanged []string  `json:"files_changed,omitempty"`
	Artifacts    []string  `json:"artifacts,omitempty"`
	ToolCallIDs  []uint64  `json:"tool_call_ids,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type TaskGuidance struct {
	MessageID uint64    `json:"message_id"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskState struct {
	ID                 uint64                `json:"id"`
	Title              string                `json:"title,omitempty"`
	Goal               string                `json:"goal"`
	Target             string                `json:"target,omitempty"`
	MayModify          bool                  `json:"may_modify"`
	ComplexityMode     string                `json:"complexity_mode"`
	Status             TaskStatus            `json:"status"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria,omitempty"`
	AcceptanceChecks   []AcceptanceCheck     `json:"acceptance_checks,omitempty"`
	Plan               Plan                  `json:"plan"`
	Context            TaskContext           `json:"context"`
	Guidance           []TaskGuidance        `json:"guidance,omitempty"`
	StepResults        []StepResult          `json:"step_results"`
	Extras             []string              `json:"extras,omitempty"`
	Failure            *TaskFailure          `json:"failure,omitempty"`
	StartedAt          time.Time             `json:"started_at"`
	CompletedAt        *time.Time            `json:"completed_at,omitempty"`
}

type MemoryKind string

const (
	MemoryFact          MemoryKind = "fact"
	MemoryDecision      MemoryKind = "decision"
	MemoryApproval      MemoryKind = "approval"
	MemoryStepResult    MemoryKind = "step_result"
	MemoryTask          MemoryKind = "task_summary"
	MemoryStructural    MemoryKind = "structural"
	MemoryGlobalConcept MemoryKind = "global_concept"
	MemoryPlan          MemoryKind = "plan"
	MemoryStep          MemoryKind = "step"
)

type MemoryItem struct {
	ID               uint64     `json:"id"`
	Kind             MemoryKind `json:"kind"`
	Text             string     `json:"text"`
	Embedding        []float32  `json:"embedding"`
	Anchors          []string   `json:"anchors,omitempty"`
	IndexKeys        []string   `json:"index_keys,omitempty"`
	FileHash         string              `json:"file_hash,omitempty"`
	SourceMessageIDs []uint64            `json:"source_message_ids,omitempty"`
	Participant      MessageSource       `json:"participant,omitempty"`
	SourceTaskID     *uint64             `json:"source_task_id,omitempty"`
	SourceStepID     *uint64             `json:"source_step_id,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
}

type TaskSummary struct {
	TaskID    uint64    `json:"task_id"`
	Summary   string    `json:"summary"`
	IndexKeys []string  `json:"index_keys,omitempty"`
	Embedding []float32 `json:"embedding"`
	CreatedAt time.Time `json:"created_at"`
}

type PendingQuestion struct {
	ID              uint64    `json:"id"`
	TaskID          *uint64   `json:"task_id,omitempty"`
	StepID          *uint64   `json:"step_id,omitempty"`
	Question        string    `json:"question"`
	Reason          string    `json:"reason"`
	ResumePrimitive string    `json:"resume_primitive"`
	AskedAt         time.Time `json:"asked_at"`
}

type ToolKind string

const (
	ToolReadFile       ToolKind = "read_file"
	ToolReadRange      ToolKind = "read_range"
	ToolSearchPath     ToolKind = "search_path"
	ToolSearchText     ToolKind = "search_text"
	ToolFindSymbol     ToolKind = "find_symbol"
	ToolListSymbols    ToolKind = "list_symbols"
	ToolReadASTNode    ToolKind = "read_ast_node"
	ToolWriteFile      ToolKind = "write_file"
	ToolPatchFile      ToolKind = "patch_file"
	ToolPatchASTNode   ToolKind = "patch_ast_node"
	ToolCreateDir      ToolKind = "create_directory"
	ToolDeleteFile     ToolKind = "delete_file"
	ToolDeleteDir      ToolKind = "delete_directory"
	ToolExecuteProcess ToolKind = "execute_process"
	ToolExecuteShell   ToolKind = "execute_shell"
	ToolGitStatus      ToolKind = "git_status"
	ToolGitDiff        ToolKind = "git_diff"
	ToolReadToolOutput ToolKind = "read_tool_output"
	ToolAnalyzeLargeFile ToolKind = "analyze_large_file"
	ToolSearchTaskSteps  ToolKind = "search_task_steps"
	ToolReadTaskStep     ToolKind = "read_task_step"
	ToolArtifactList     ToolKind = "artifact_list"
	ToolArtifactRead     ToolKind = "artifact_read"
	ToolArtifactWrite    ToolKind = "artifact_write"
	ToolArtifactAppend   ToolKind = "artifact_append"
	ToolArtifactPatch    ToolKind = "artifact_patch"
	ToolArtifactSearch   ToolKind = "artifact_search"
)

type ToolCall struct {
	ID               uint64    `json:"id"`
	Kind             ToolKind  `json:"kind"`
	Description      string    `json:"description"`
	Input            any       `json:"input"`
	RequiresApproval bool      `json:"requires_approval"`
	AllowRunEligible bool      `json:"allow_run_eligible,omitempty"`
	Approved         *bool     `json:"approved,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

type ToolResult struct {
	ToolCallID uint64    `json:"tool_call_id"`
	Success    bool      `json:"success"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	Truncated  bool      `json:"truncated,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type LogKind string

const (
	LogPrimitive  LogKind = "primitive"
	LogLLM        LogKind = "llm"
	LogRouting    LogKind = "routing"
	LogTool       LogKind = "tool"
	LogPermission LogKind = "permission"
	LogRepair     LogKind = "repair"
	LogPlan       LogKind = "plan"
	LogSystem     LogKind = "system"
)

type LogCategory string

const (
	CategoryTool         LogCategory = "tool"
	CategoryMemoryLookup LogCategory = "memory_lookup"
	CategoryIndexing     LogCategory = "indexing"
	CategoryNegotiation  LogCategory = "negotiation"
	CategorySystem       LogCategory = "system"
	CategoryPlan         LogCategory = "plan"
)

type ExecutionLogEntry struct {
	ID         uint64      `json:"id"`
	Kind       LogKind     `json:"kind"`
	Category   LogCategory `json:"category"`
	TaskID     *uint64   `json:"task_id,omitempty"`
	StepID     *uint64   `json:"step_id,omitempty"`
	Primitive  string    `json:"primitive,omitempty"`
	Summary    string    `json:"summary"`
	Input      string    `json:"input,omitempty"`
	RawOutput  string    `json:"raw_output,omitempty"`
	ParsedData string    `json:"parsed_data,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type RuntimeStats struct {
	Inferences           uint64            `json:"inferences"`
	Embeddings           uint64            `json:"embeddings"`
	InferenceByPrimitive map[string]uint64 `json:"inference_by_primitive,omitempty"`
}

type UploadState struct {
	ID           string    `json:"id"`
	Filename     string    `json:"filename"`
	OriginalPath string    `json:"original_path,omitempty"`
	ReadOnly     bool      `json:"read_only"`
	CreatedAt    time.Time `json:"created_at"`
}

type AttachmentType string

const (
	AttachmentTypeUpload   AttachmentType = "upload"
	AttachmentTypeArtifact AttachmentType = "artifact"
)

type AttachmentState struct {
	ID          string         `json:"id"` // Either Upload ID or Artifact Path
	VirtualPath string         `json:"virtual_path"`
	Type        AttachmentType `json:"type"`
	CreatedAt   time.Time      `json:"created_at"`
}

type AgentState struct {
	Version           int                 `json:"version"`
	SessionID         string              `json:"session_id,omitempty"`
	SessionTitle      string              `json:"session_title,omitempty"`
	CreatedAt         time.Time           `json:"created_at,omitempty"`
	UpdatedAt         time.Time           `json:"updated_at,omitempty"`
	Messages          []Message           `json:"messages"`
	ExecutionLog      []ExecutionLogEntry `json:"execution_log"`
	ToolCalls         []ToolCall          `json:"tool_calls"`
	ToolResults       []ToolResult        `json:"tool_results"`
	CompletedTasks    []TaskState         `json:"completed_tasks"`
	ActiveTask        *TaskState          `json:"active_task,omitempty"`
	PendingQuestion   *PendingQuestion    `json:"pending_question,omitempty"`
	Uploads           []UploadState       `json:"uploads,omitempty"`
	ActiveAttachments []AttachmentState   `json:"active_attachments,omitempty"`
	ComplexityMode    string              `json:"complexity_mode,omitempty"`
	ReadOnly          bool                `json:"read_only,omitempty"`
	NextID            uint64              `json:"next_id"`
	Sequence          uint64              `json:"sequence"`
}

func NewAgentState() *AgentState {
	return &AgentState{Version: 2, NextID: 1, ComplexityMode: "Fast"}
}

func (s *AgentState) ID() uint64 {
	id := s.NextID
	s.NextID++
	return id
}
