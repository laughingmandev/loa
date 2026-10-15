# Settings Reference Guide

The Web UI Settings modal currently contains five tabs. This guide describes the controls that exist in the current `main` UI and how they map to the runtime configuration.

Project overrides are stored in `.loa.json`. Global configuration is stored under the OS user-config directory in `loa/config.json` (for a typical Linux installation, `~/.config/loa/config.json`).

## 1. Model & Limits

![Model & Limits Tab](settings_models.png)

### Model Endpoint

- **API Base URL**: base address of the OpenAI-compatible model server.
- **API Key**: optional Bearer token.
- **Use one model for all inferences**: UI convenience for assigning one selected model across Loa's model roles.
- **Crawling model**: used by crawler primitives such as Narrative Frame/keyword generation.
- **Conversation model**: used by lightweight/background/control primitives where the engine selects the conversation role.
- **Planning model**: used by planning-related primitives.
- **Executing model**: used by execution/tool-decision primitives and some analysis helpers.

Loa's current client expects `/v1/models` and `/v1/chat/completions` from the configured model server.

### Embeddings

- **Embedding Engine: local**: use the GGUF local CPU embedding backend compiled into the normal release/localembed build.
- **Embedding Engine: openapi**: call the configured OpenAI-compatible `/v1/embeddings` endpoint.
- **Embedding Model**: external embedding model name when `openapi` is selected.
- **Local Embedding Model Path**: GGUF file used by the local embedding engine.

### Execution/Evaluation Limits

- **JSON Repair Attempts**: maximum number of model repair attempts after strict structured-output parsing/validation fails.
- **Evaluation Mode**:
  - UI **Strict** -> stored as `Immediate`.
  - UI **Dynamic** -> stored as `Dynamic`.
- **Blind Action Threshold**: in Dynamic mode, the count at which the engine invokes `AssessBlindExecution` as a soft decision point. It is not a continuously running supervisor.
- **Max Execution Loops**: bounds internal execution loops, including per-step execution/recovery paths. It is not one global counter for all operations across a full task.
- **Max Plan Depth**: used as a bound across several plan/replan/decomposition/final-audit paths; it is broader than only visual DAG depth.
- **Max Prerequisites Per Step**: configures the maximum number of direct dependencies evaluated and injected as prerequisites per plan step.

*(Note on `Force JSON Mode`: The `force_json_mode` setting exists to bypass Ollama's fragile `json_object` parser on affected models. It defaults to `true`. If you experience 0-length silent crashes on Qwen models, you can disable it to handle JSON strictly through Loa's application layer. It is intentionally excluded from the UI to prevent accidental LLM crashes for users who need it enabled. It can only be changed by manually editing the global config file.)*

### Timeouts

- **Model Timeout Seconds**
- **Command Timeout Seconds**
- **LLM Polling Timeout Seconds**
- **Server Read Header Timeout Seconds**

See the field-level [Settings Reference](../reference/settings.md) for defaults.

> [!IMPORTANT]
> None of these settings limits the **total number of model requests** made by a task. Loa is intentionally inference-heavy. A metered cloud provider can consume credits/rate limits quickly even when every individual request stays inside `context_budget`.

## 2. File Indexing & Tools

![File Indexing Tab](settings_indexing.png)

Current controls are:

- **FS Watcher Debounce (ms)**: coalesces filesystem events before files are marked stale.
- **Max Tool Output Bytes**: maximum tool result size retained by the Tool Manager before it truncates the result and marks it `Truncated`.
- **Crawler Small File Threshold**: line threshold below which a source file can be processed as one crawler chunk instead of symbol-oriented chunking.
- **Crawler Max Chunk Lines**: target upper bound used when the crawler groups source/index blocks into Narrative Frame chunks.
- **`.loaignore`**: project-specific paths ignored by indexing/watcher logic.

The current tab does **not** expose a `Use AST Search` toggle, generic `Max File Size`, or indexing `Top-K Results` control.

## 3. Memory Physics

![Memory Physics Tab](settings_physics.png)

The current tab configures retrieval/scoring behavior. It does not contain a memory-compression ratio or a maximum DAG-history compression setting.

- **Memory Top-K**: number of final retrieved memory results requested by search.
- **Memory Candidate Pool**: initial candidate count before reranking/filtering.
- **Project/LTS Decay Half-Life (Hours)**: age-based relevance-decay parameter used when scoring project memory.
- **Project/LTS Max Weight**: maximum age/relevance weight contribution for project memory.
- **Session Decay Half-Life (Hours)**: age-based relevance-decay parameter used for the session store.
- **Session Max Weight**: corresponding session-memory maximum weight.
- **Memory Rerank Gates**: configured number of reranking/filter gates.
- **Memory Pool Expansion Limit**: configured limit for expanding the candidate pool when retrieval needs more candidates.

These half-life values affect retrieval scoring. They do not mean memories are automatically deleted or rewritten into compressed summaries when the half-life expires.

## 4. Context Synthesis

![Context Synthesis Tab](settings_context.png)

### Total Context Budget

`context_budget` is the maximum context size used when composing an individual inference. It is **not** a total task budget.

For non-OpenAI-host URLs, the current client also sends this value to the chat endpoint as `options.num_ctx`.

### Output Reserve

`output_reserve` is reserved from the context budget for model output and is sent as the chat-completions `max_tokens` value.

### Code Budget

`code_budget` limits the amount of extra retrieved/tool material that the context builder can add. It is not a separate model context window.

### Recent Messages

`recent_messages` (default 10) is the maximum, and the default, number of chat messages a single `read_history` call returns. Chat history is not injected into prompts; agents read it on demand.

### Recent Task Breadcrumbs

`context_recent_breadcrumbs` controls how many recent completed-step summaries can be included as breadcrumbs.

Direct DAG prerequisites are separate: completed results from the current step's direct `depends_on` dependencies are injected as direct prerequisite context.

### Context Margin Percentages

The builder reserves safety buffers to ensure prompt generation stays within limits. `context_network_margin_percent` (labeled **Context Allocation Margin %** in the Web UI, default 2%) is applied to avoid absolute hardware/backend boundaries. `context_prompt_margin_percent` (labeled **Context Prompt Margin %**, default 15%) is applied on top of that to absorb heuristic variance during local token counting.

### Code Budget Max Floor

Despite the historical/UI-oriented name, `context_code_budget_max_floor` is currently used as a lower floor on the builder's computed `maxRunes` prompt allowance after subtracting the output reserve. It is not a standalone guaranteed code-injection allocation.

### Max Prompt Tool Output Bytes

`max_prompt_tool_output_bytes` exists in the current configuration and UI. During this documentation audit, no active consumer of this field was found in the current engine or Tool Manager path. The Tool Manager does enforce `max_stored_tool_output_bytes`; documentation should therefore not currently claim that `max_prompt_tool_output_bytes` automatically spills oversized output into an artifact.

### Persistent Project Instructions

`project_instructions` is inserted into the mandatory model context when non-empty.

## 5. Tools & Permissions

![Tools & Permissions Tab](settings_tools.png)

Permission modes:

- **ask every tool** -> `ask_all`
- **ask selected tools** -> `ask_selected`
- **allow all** -> `allow_all`

The tool grid determines which tools trigger approval in `ask_selected` mode.

The default configuration is `ask_selected` with mutating/process/shell tools included in the approval set.

Permissions are approval controls. They do not sandbox shell syntax. `execute_process` and `execute_shell` run with the filesystem/OS access of the environment where Loa is executing; only the registered file tools apply Loa's project-root path boundary.
