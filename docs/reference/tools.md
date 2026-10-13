# Tooling Reference

Loa exposes a fixed Tool Manager surface from `internal/tools/manager.go`. Model primitives request tools through structured payloads; the Tool Manager performs the operation and returns a `ToolResult`.

Tool permissions and task read-only state determine whether a requested tool is available/approved. Tool categories below describe implementation behavior, not a blanket safety guarantee.

## 1. File Read / Search Tools

*Note on binary files*: The Tool Manager applies a dynamic binary sniffing heuristic (inspecting the first 512 bytes for null bytes) rather than relying on file extensions. Tools below will automatically reject files identified as binary to protect the LLM context.

### `read_file`

Reads a complete project-relative file.

```json
{"path":"main.go"}
```

### `read_range`

Reads a line range from a project-relative file.

```json
{"path":"main.go","start":10,"end":40}
```

### `search_path`

Searches project paths by name fragment.

```json
{"query":"*.go","limit":50}
```

### `search_text`

Literal or regex text search.

```json
{"pattern":"TODO:","regex":false,"limit":50}
```

### `find_symbol`

Queries the code index for a symbol.

```json
{"query":"Manager","limit":10}
```

### `list_symbols`

Lists indexed symbols for one project-relative file.

```json
{"path":"internal/tools/manager.go"}
```

### `read_ast_node`

Runs an `ast-grep` rule over a project file and returns matching source.

```json
{"path":"main.go","rule":"{pattern: 'func $NAME() { $$$BODY }'}"}
```

The current AST-tool implementation constructs its ast-grep rule with language `go`; it should not be documented as a language-agnostic structural parser in its current form.

## 2. File Mutation Tools

These are classified as mutating.

### `write_file`

```json
{"path":"main.go","content":"package main\n"}
```

### `patch_file`

```json
{"path":"main.go","old":"old text","new":"new text","replace_all":false}
```

### `patch_ast_node`

```json
{"path":"main.go","rule":"{pattern: 'func $NAME() { $$$BODY }'}","rewrite":"func $NAME() error { return nil }"}
```

Like `read_ast_node`, the current implementation is Go-oriented.

### `create_directory`

```json
{"path":"internal/newpkg"}
```

### `delete_file`

```json
{"path":"temp.txt"}
```

### `delete_directory`

```json
{"path":"build"}
```

Delete tools are additionally classified as destructive. The directory-delete implementation refuses to delete the project root itself.

## 3. Process Execution

### `execute_process`

Executes a binary directly with arguments.

```json
{"binary":"go","args":["test","./..."],"timeout_seconds":0}
```

### `execute_shell`

Executes through `/bin/sh -c`.

```json
{"command":"go test ./... | tee test.log","timeout_seconds":0}
```

Both are classified as mutating for permissions. They start in the project root, but unlike Loa's file tools they are not restricted by project-root path validation. Their filesystem/OS access is determined by the environment in which Loa is running (host or sandbox).

A non-positive per-call timeout uses the configured command timeout.

## 4. Git Tools

### `git_status`

Runs:

```text
git status --short --branch
```

### `git_diff`

```json
{"staged":false}
```

With `staged: true`, Loa runs `git diff --cached`.

These wrappers execute Git as a child process in the project root.

## 5. Stored Tool Output

### `read_tool_output`

Reads a byte range from the Tool Manager's stored output for a prior call.

```json
{"tool_call_id":123,"offset":0,"limit":16384}
```

Important: ordinary tool output is truncated to `max_stored_tool_output_bytes` **before** it is stored in the manager's output map. `read_tool_output` can page through the stored value; it cannot recover bytes already discarded by that truncation.

## 6. Large File Analysis

### `analyze_large_file`

```json
{"path":"large.log","instructions":"Identify the first failure and its likely cause"}
```

This is not a simple deterministic file reader. The current implementation reads the file in 500-line chunks and invokes the configured executing model iteratively, passing the previous summary and the next chunk to produce a running analysis. It is exempt from the Tool Manager's normal stored-output truncation branch.

This tool is an explicit model-driven summarization/analysis mechanism for a requested large-file operation. It is separate from Loa's core context-window strategy.

## 7. Artifacts

Artifacts live under the active task/session artifact area.

- `artifact_list`
- `artifact_read`
- `artifact_write`
- `artifact_append`
- `artifact_patch`
- `artifact_search`

Write/append/patch artifact tools are omitted from the read-only tool schema.

## 8. Task-Step Introspection

### `search_task_steps`

Searches step information for the active task.

### `read_task_step`

```json
{"step_id":123}
```

Reads stored information for one task step.

## 9. Project-Root Validation

Filesystem tools require project-relative paths. Existing paths are resolved through symlinks and checked against the resolved project root. Creation resolves/checks the real parent before constructing the target path.

Attempts to use an absolute path or escape the project root through traversal/symlinks are rejected by these file-tool helpers.

This path boundary does not apply to arbitrary commands launched by `execute_process`/`execute_shell`.

## 10. Output Truncation and Sanitization

For ordinary tools, the Tool Manager uses `max_stored_tool_output_bytes` (default 4 MiB). If output exceeds the configured limit:

1. the output string is cut to the limit,
2. `ToolResult.Truncated` is set,
3. the truncated value is stored as the tool output.

Additionally, the Tool Manager applies a `sanitizeOutput` filter to strip unprintable or corrupt characters from tool results before they are passed into the LLM context buffer.

Documentation should not currently claim a second automatic prompt-output spill-to-artifact stage based on `max_prompt_tool_output_bytes`; that configuration field exists, but this audit found no active runtime consumer in the current engine/Tool Manager path.
