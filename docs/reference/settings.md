# Settings Reference

Loa configuration is loaded in this order:

1. built-in defaults,
2. global config from the OS user-config directory: `loa/config.json`,
3. project overrides from `<project>/.loa.json`.

On a typical Linux installation, the global path resolves to `~/.config/loa/config.json`.

The table below reflects the current `internal/config/config.go` fields/defaults.

## Model and Endpoint

| JSON field | Default | Current use |
| --- | --- | --- |
| `ollama_url` | `http://127.0.0.1:11434` | Base URL for the OpenAI-compatible model client. |
| `api_key` | empty | Optional Bearer token. |
| `model_crawling` | empty | Model role used by crawler primitives. |
| `model_conversation` | empty | Conversation/control model role. |
| `model_planning` | empty | Planning model role. |
| `model_executing` | empty | Execution/tool-decision model role. |
| `embedding_engine` | `local` | `local` GGUF embedding backend or external OpenAI-compatible embeddings. |
| `embedding_model` | empty | External embedding model identifier. |
| `local_embedding_model_path` | `~/.loa/embedding-models/nomic-embed-text-v1.5.f16.gguf` (expanded absolute default internally) | Local GGUF embedding model. |

The current model client uses `/v1/models`, `/v1/chat/completions`, and `/v1/embeddings` (when external embeddings are used).

## Context

| JSON field | Default | Current use |
| --- | ---: | --- |
| `context_budget` | `100000` | Per-inference context budget used by the context builder. For non-OpenAI-host URLs it is also sent as `options.num_ctx`. |
| `output_reserve` | `4000` | Reserved output allowance; sent as chat `max_tokens`. |
| `code_budget` | `8000` | Rune/token allowance limiting extra retrieved/tool context injected by the builder. |
| `recent_messages` | `10` | Maximum (and default) number of chat messages one `read_history` call returns. Chat history is not injected into prompts; agents pull it via `read_history`. |
| `context_recent_breadcrumbs` | `5` | Number of recent completed-step summaries injected as breadcrumbs. |
| `context_prompt_margin_percent` | `15` | Percentage of budget shaved off to account for local token-counting heuristics. |
| `context_network_margin_percent` | `2` | Percentage of budget shaved off to avoid hardware/backend context boundaries. |
| `context_code_budget_max_floor` | `8000` | Current lower floor on the builder's computed `maxRunes` prompt allowance after output reserve is subtracted. |
| `project_instructions` | empty | Added to mandatory task context when non-empty. |

`context_budget` is not a task-level cost/request limit. A task can perform many independent inferences, each of which stays within this budget.

## Memory Retrieval

| JSON field | Default | Current use |
| --- | ---: | --- |
| `memory_top_k` | `8` | Final requested memory-search result count. |
| `memory_candidate_pool` | `30` | Initial retrieval candidate pool. |
| `memory_lts_decay_half_life_hours` | `336.0` | Project-memory age-decay half-life used in retrieval scoring. |
| `memory_lts_max_weight` | `0.03` | Project-memory maximum age/relevance weight contribution. |
| `memory_session_decay_half_life_hours` | `2.0` | Session-memory age-decay half-life used in retrieval scoring. |
| `memory_session_max_weight` | `0.20` | Session-memory maximum age/relevance weight contribution. |
| `memory_rerank_gates` | `3` | Configured reranking/filter gate count. |
| `memory_pool_expansion_limit` | `4` | Configured limit for candidate-pool expansion. |

These decay settings affect search scoring. They do not define a memory-deletion or recursive compression schedule.

## Planning, Evaluation, and Limits

| JSON field | Default | Current use |
| --- | ---: | --- |
| `json_repair_attempts` | `7` | Maximum model repair attempts for malformed/invalid structured output. |
| `max_plan_depth` | `25` | Bound reused across plan/replan attempts, decomposition depth, and final iterative verification. |
| `max_execution_loops` | `80` | Bound used by internal execution loops, including per-step execution/recovery and negotiation loops. |
| `plan_research_max_loops` | `6` | Maximum actions in the pre-planning research loop (memories, task summaries, attached files, chat history). When reached, planning proceeds with what was collected. Config file only; not in the settings UI yet. |
| `blind_action_threshold` | `8` | Dynamic-mode threshold that triggers `AssessBlindExecution`. |
| `evaluation_mode` | `Dynamic` | `Dynamic` or `Immediate` (the UI labels `Immediate` as Strict). |
| `max_prerequisites_per_step` | `4` | Maximum number of prerequisites (direct dependencies) evaluated and injected per step. |
| `force_json_mode` | `true` | Enforces strict API-level JSON output. See note below. |

**Note on `force_json_mode`**: This setting is intentionally absent from the current Web UI and must be changed via manual config edit if needed. When `true` (default), Loa sends the `"response_format": {"type": "json_object"}` payload to the LLM backend to enforce strict JSON grammar. If you experience a known Ollama bug where evaluating very large prompts on certain models (like Qwen) causes a 0-length silent crash, you can set this to `false` to bypass the fragile parser and handle JSON extraction entirely through Loa's robust application-layer logic.

`max_execution_loops` should not be interpreted as one global counter across the complete lifetime of a task.

## Crawler and Filesystem Watcher

| JSON field | Default | Current use |
| --- | ---: | --- |
| `crawler_small_file_threshold` | `200` | Line threshold used by crawler chunking. |
| `crawler_max_chunk_lines` | `150` | Crawler chunk-size target/limit used when grouping indexed blocks. |
| `fs_watcher_debounce_ms` | `500` | Filesystem event debounce before changed files are marked stale. |

## Tool Output

| JSON field | Default | Current use |
| --- | ---: | --- |
| `max_stored_tool_output_bytes` | `4194304` | Tool Manager truncates ordinary tool result output to this many bytes and marks the result `Truncated`. `analyze_large_file` is exempt. |
| `max_prompt_tool_output_bytes` | `30000` | Field/UI control exists, but this audit found no active consumer in the current engine or Tool Manager path. |

Because the Tool Manager stores the already-truncated output in its internal output map, `read_tool_output` cannot recover bytes discarded by `max_stored_tool_output_bytes`.

## Timeouts

| JSON field | Default | Current use |
| --- | ---: | --- |
| `model_timeout_seconds` | `600` | Model-request timeout. |
| `command_timeout_seconds` | `120` | Default process/shell timeout when a tool call does not provide a positive override. |
| `llm_polling_timeout_seconds` | `15` | LLM polling timeout configuration. |
| `server_read_header_timeout_seconds` | `10` | HTTP server read-header timeout. |

## Permissions

Configuration shape:

```json
{
  "permissions": {
    "mode": "ask_selected",
    "ask_for": {
      "write_file": true,
      "patch_file": true,
      "delete_file": true,
      "delete_directory": true,
      "execute_process": true,
      "execute_shell": true
    }
  }
}
```

Permission modes:

- `ask_all`
- `ask_selected`
- `allow_all`

The default mode is `ask_selected`. The default approval set includes write/patch/delete/process/shell operations. `patch_ast_node` and `create_directory` are classified as mutating by the Tool Manager but are not included in the default `ask_for` map shown by `config.Default()`.

The approval model is independent from task read-only enforcement. The engine/tool schema can also withhold mutating tools for read-only contexts.

## Normalization Notes

Most positive numeric fields are restored to their documented defaults when loaded with a non-positive value, but normalization is not uniform for every field. The definitive behavior is `Config.normalize()` in `internal/config/config.go`; do not assume every zero value is automatically replaced.
