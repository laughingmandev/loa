# Loa Usage Guide

This guide covers first-time setup, the current Web UI, execution modes, steering, pause/resume, and the operational differences between local/self-hosted and metered inference.

## First-Time Setup

Loa has two setup stages: global model/runtime configuration and project-specific setup.

### 1. Initial Setup Wizard

The initial setup wizard configures:

- API base URL and optional API key,
- model roles for Crawling, Conversation, Planning, and Executing,
- embedding engine (`local` or external OpenAI-compatible API),
- embedding model/path,
- total context budget.

![Initial Setup Wizard](initial_setup.png)

For a first run, using one chat model for all model roles is the simplest configuration. The model endpoint must provide the OpenAI-compatible endpoints Loa uses (`/v1/models` and `/v1/chat/completions`). If external embeddings are selected, `/v1/embeddings` must also be available.

Use **TEST CONFIGURATION** before completing setup.

### Local CPU Embeddings

If you downloaded a GGUF embedding model through `setup.sh`, select the local embedding engine and choose the model path. Local embedding paths are stored using a `~/`-relative form where possible so the same global configuration can also resolve inside `loa-sandbox`.

### Context Budget

`Total Context Budget` is the maximum context size Loa uses when composing an **individual model request**. It is not a total token or request budget for a task.

Set it to a value supported by both your inference server and the model you are running. Loa reserves space for model output and applies its own safety buffer when composing prompts.

> [!IMPORTANT]
> **Metered cloud inference is not the intended deployment model.**
>
> Project crawling itself can invoke the model repeatedly: unindexed source chunks receive a Narrative Frame and keyword extraction. Fast Mode is lighter than Planned Mode but still uses Loa's state/retrieval/memory machinery. Planned Mode can generate a large number of independent model calls for planning, execution, evaluation, recovery, context maintenance, and verification.
>
> A cloud OpenAI-compatible endpoint can work technically, but API credits and rate limits can be consumed very quickly.

### Sandbox and Host State

When Loa runs through `loa-sandbox`, the container receives several host mounts:

- the current project at `/workspace` read/write,
- host `~/.loa` read-only,
- host `~/.config/loa` read-only when that directory exists.

If the host global-config directory does not exist, the launcher provisions sandbox-local writable global config storage.

Project configuration is stored at `.loa.json` in the project root. Project runtime state is stored under `.loa/`.

See [Sandbox and Security](../core_concepts/sandbox_and_security.md) for the complete boundary.

### 2. Project Setup Wizard

The project setup wizard appears when Loa initializes a project without an existing session/project setup.

![Project Setup Wizard](project_setup.png)

The current wizard includes:

- **Initial Session Title**
- **Project Purpose (Global Concept Memory)**
- **`.loaignore`**
- **START BOOT SCAN**

`.loaignore` controls which paths are excluded from the file/symbol index, crawler, and filesystem watcher (alongside Loa's built-in exclusions for hidden paths, dependency/build folders, and binary files). Because Loa indexes all non-ignored, non-empty UTF-8 text files—including symbol-less markdown, plain text, and configuration files—choose `.loaignore` before the first boot scan on large repositories to avoid unnecessary `NarrativeFrame` model calls.

The project purpose is stored as project knowledge so the agent has a stable high-level description of the repository.

## Settings and Context Sizing

The Web UI Settings modal contains five tabs:

1. Model & Limits
2. File Indexing & Tools
3. Memory Physics
4. Context Synthesis
5. Tools & Permissions

![Settings Dialog - Context Synthesis](settings_context.png)

There is no universal context-budget value that can be derived from VRAM alone. The usable limit depends on the model, quantization/runtime, inference-server configuration, and the model's supported context length.

For the exact fields and current semantics, see [Settings Reference Guide](settings_reference.md).

## Web UI Overview

### 1. Chat

The Chat area is the user-facing task interface.

You can:

- submit conversational or coding requests,
- select Auto, Fast, or Planned execution behavior,
- approve modifying Planned tasks,
- send steering/intervention messages while a task is running,
- attach uploaded files/artifacts.

### 2. Inspect Pane

The right-side inspect pane is split into three top-level groups.

#### SESSION

- **PLAN:** current/historical task plan state.
- **GRAPH:** DAG/dependency view. For Planned tasks, dependencies also affect which completed step results become direct context prerequisites.
- **UPLOADS:** session-scoped uploaded files.

#### MEMORY

- **WORKING MEMORY:** active task facts, decisions, constraints, open issues, and related task context.
- **SESSION MEMORY:** session-scoped retrievable memories/task summaries.
- **PROJECT MEMORY:** project-level persistent memories and live-synced project file tree, including structural file memories produced by the crawler during Boot Scan, idle watcher auto-crawls, Just-In-Time memory retrieval, or task completion.

#### OUTPUTS

- **ARTIFACTS:** task/step artifacts written by the agent.
- **LAST CONTEXT:** the last context payload assembled for a model inference.
- **EXECUTION LOG:** recorded primitive/tool activity, including model primitive inputs/outputs and tool results.

The Execution Log is an implementation/audit log. It should not be interpreted as access to an unrecorded internal chain of thought beyond what the Loa primitives themselves explicitly emit and persist.

## Session Uploads and Attachments

The **UPLOADS** tab stores external session files under Loa's project-local runtime area.

A manually uploaded file can be attached to the current conversation. Read-only uploads are restored from their master copy if the working attachment is modified; writable uploads synchronize working-copy changes back to the master upload.

Artifacts can also be attached as read-only context.

These files are exposed to the model through virtual/working paths when attached; detaching them removes them from active prompt context without deleting the stored upload.

See [Sandbox and Security](../core_concepts/sandbox_and_security.md) for the trust boundary.

## Execution Modes

### Auto Mode

Auto Mode runs Loa's intent/ambiguity path and chooses the execution route based on the request.

If you already know the request should be a quick local task or a full planned task, selecting Fast or Planned explicitly avoids relying on automatic mode selection.

### Fast Mode

Fast Mode skips formal acceptance-criteria generation and the initial multi-step DAG planning/approval flow. Internally, the current engine creates one synthetic `Execute Fast Track` step and executes it through Loa's normal tool/retrieval/state machinery.

Use Fast Mode for work where a full DAG is unnecessary, such as localized inspection, small edits, or direct commands.

Fast Mode is **not** equivalent to one model call. It can still use retrieval, tools, multiple execution decisions, and task-memory finalization.

### Planned Mode

Planned Mode is intended for larger work where explicit acceptance criteria and a mutable multi-step plan are useful.

The engine:

1. derives acceptance criteria,
2. maps criteria dependencies,
3. generates/evaluates a DAG plan,
4. waits for approval if the plan is modifying,
5. executes/evaluates the steps,
6. can repair/replan when execution invalidates the current approach,
7. runs a final acceptance audit before synthesis.

## Prompting

Loa performs best when the user supplies the constraints that actually define success.

For modifying work, useful prompt information includes:

- the intended outcome,
- architectural constraints/invariants,
- acceptance criteria,
- relevant files/components when known,
- requirements that must not change.

You do not need to pre-design every implementation step; the Planned path exists to investigate and construct that plan. But constraints that are important to you should be stated explicitly rather than left to product-taste inference.

## Steering During Execution

You can send a message while a task is running.

The message is queued as an intervention. The intervention primitive can update task facts, decisions, constraints, or open issues.

For Planned Mode, the intervention can trigger replanning of the remaining DAG. In Fast Mode there is no multi-step DAG remainder to structurally replan, so the guidance is incorporated into the continuing Fast execution.

## Dynamic Evaluation

The current settings UI exposes two evaluation modes:

- **Strict** (stored internally as `Immediate`)
- **Dynamic**

Strict evaluation takes the immediate evaluation path for eligible mutating actions.

Dynamic mode allows eligible mutating actions to continue without a full immediate evaluation when the model requests that. The engine counts these blind actions. When `Blind Action Threshold` is reached, `AssessBlindExecution` is invoked and decides whether a full evaluation should occur or execution can continue.

This is a threshold-triggered decision point, not a continuously running background supervisor.

## Pause, Resume, and Recovery

Loa persists session/task state under `.loa/`.

### Manual Pause

Pausing cancels the active run and records the task as paused while keeping the persisted state needed for inspection/resume.

### Error Pause

If execution cannot continue, the engine can pause with an error and return a currently-running step to a pending state for later recovery.

### Restart

If Loa is stopped while a task is persisted as `running`, loading that session converts the task to `paused`. It can then be inspected and resumed.

This makes it possible to stop the process/computer and continue a long-running task later, assuming the underlying project/environment still matches what the task expects.

## Host vs. Sandbox Execution

### Host

```bash
loa .
```

The default host bind address is `127.0.0.1:7171`.

`execute_process` and `execute_shell` run with your host user privileges in this mode.

### Sandbox

```bash
loa-sandbox
```

The sandbox exposes the UI on port `8104` and mounts the current project at `/workspace` read/write.

File tools are project-root constrained in both modes. Shell/process tools are not restricted by that file-tool boundary; their effective filesystem access is determined by the environment Loa is running in.
