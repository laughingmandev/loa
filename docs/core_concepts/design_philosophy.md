# Design Philosophy

The strongest case for Loa is not simply that it uses an LLM to edit code. The defining part is the control system built around the model.

Loa is intended for architect-driven engineering rather than autonomous product taste. You provide the goal, architectural intent, constraints, and acceptance expectations; Loa provides a stateful mechanism for investigation, implementation, verification, recovery, and long-running execution underneath that direction.

The principles below describe the current implementation rather than a generic agent architecture.

## 1. Mutable State and Planning

- **A mutable DAG instead of a fixed plan.** Planned Mode starts with an explicit plan, but the plan can be decomposed or replanned as execution discovers new information.
- **Dependencies are also context topology.** A DAG edge affects execution eligibility and which completed `StepResult` values are injected as direct prerequisites for a dependent step.
- **Bounded operations instead of one giant inference.** Loa deliberately separates intent, criteria, planning, execution decisions, hypotheses, evaluation, recovery, context maintenance, and verification into smaller operations.
- **Sequential task execution where state matters.** Plan steps are not executed as independent parallel agents. The current task is progressed through a serialized state machine so mutations and later reasoning observe the current persisted state.
- **Plan approval before Planned mutations.** A modifying Planned task waits for explicit plan approval before execution. Fast Mode deliberately skips the initial multi-step planning/approval path.

## 2. Execution, Evaluation, and Recovery

- **Model decisions and tool execution are separate.** `ExecuteStep` decides what should happen next; the Tool Manager performs the requested operation and returns a tool result.
- **Hypothesis before many mutations.** Mutating tools other than `execute_process` can be preceded by `FormulateHypothesis` in the current engine.
- **"Ran" is not the same as "verified."** A successful tool exit is not automatically treated as proof that a step's objective is complete. Evaluation can return partial or verification-required outcomes.
- **Failure is structured state.** The recovery path can distinguish retry, investigate/repair, clarification, rollback/recovery, and replanning rather than applying a single retry rule.
- **Malformed structured output is repaired through bounded model retries.** The LLM client parses JSON strictly and can ask the model to repair malformed output up to the configured retry limit. The current implementation accounts for **dynamic repair accounting**, dynamically calculating the token overhead of a repair stack trace and subtracting it from the available context window to prevent catastrophic buffer overflows during complex loops. It also provides a config toggle (`force_json_mode`) to bypass fragile backend JSON parsers (e.g., Ollama's 0-length silent crashes on Qwen models) to handle JSON extraction entirely through robust application-layer logic if needed. The current implementation should not be described as having a separate deterministic JSON-repair pipeline.

## 3. Context and Memory

- **Context is reconstructed for each inference.** Loa composes the active task/step state, direct DAG prerequisites, recent breadcrumbs/messages, and selected extra context against the current token budget.
- **The context window is not the memory store.** Persisted project memory, session memory, task state, tool results, artifacts, and source files remain separate from the prompt assembled for one inference.
- **Original evidence is not recursively rewritten to fit the context window.** When material does not fit, the context builder selects/truncates what is injected. It does not repeatedly summarize the whole previous prompt into a new canonical version.
- **Derived memories have explicit roles.** Narrative Frames, `StepResult` extraction, task summaries, refreshed task context, and consolidated semantic memories are derived representations used for retrieval/control. They do not erase the underlying source/task/tool records.
- **Project structural memories are file-anchored.** The filesystem watcher and file tools invalidate memories for changed files, rebuilding them immediately in the background when Loa is idle or reconciling them lazily before memory retrieval and at task completion when a task is running.

## 4. Autonomy and Observability

- **Long-running autonomy is a first-class target.** Planned tasks are designed to survive many tool calls, evaluations, failures, and replans rather than assuming a short interactive exchange.
- **High inference count is intentional.** Local/self-hosted inference makes it practical to spend model calls on independent checking, retrieval, evaluation, and recovery. Loa is not designed around minimizing billable API requests.
- **Pause and resume preserve task state.** A paused/error-stopped task keeps its persisted DAG/session/tool state. After process restart, a previously running task is loaded as paused and can be inspected/resumed.
- **User steering is part of execution.** Messages received during a running task are handled as interventions. Planned Mode can replan its remaining DAG around new guidance.
- **The control state is inspectable.** The UI exposes plan/graph state, memory views, the last composed model context, artifacts, execution logs, approvals, uploads, and task controls.

## 5. Security and Trust Boundaries

- **The permission model and the sandbox are separate controls.** `ask_all`, `ask_selected`, and `allow_all` decide when tool calls require approval. Docker decides which host resources are exposed to a sandboxed Loa process.
- **File tools are project-root constrained.** Registered file read/write/delete operations validate relative paths and reject traversal outside the project root.
- **Shell/process tools are deliberately more powerful.** `execute_process` and `execute_shell` execute in the operating environment where Loa is running and are not confined by the file-tool project-root checks.
- **The Docker sandbox still mounts the target project read/write.** It protects the rest of the host through container isolation/mount choices; it does not make the project itself immutable.

See [Sandbox and Security](sandbox_and_security.md) for the concrete boundaries.

## 6. Model-Agnostic Control Layer

Loa talks to an OpenAI-compatible model endpoint and keeps the DAG, persisted task state, memory stores, context builder, permissions, tools, and recovery logic outside the model.

A stronger model can improve the quality of decisions and code generation, but the surrounding control structure is implemented by Loa rather than being delegated to a provider-specific agent runtime.
