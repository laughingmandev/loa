# Primitives Reference

Primitives are model-backed control operations used by Loa's engine/crawler. They are not the same thing as tools: a primitive can decide to request a tool, while the Tool Manager executes the requested operation separately.

This reference describes primitives that are actively invoked by the current `main` control paths. Exact implementation structs live under `internal/agent/primitives/`.

## 1. Intent, Clarification, and Negotiation

- **`Intent`**: classifies an Auto-mode user message. If the result contains task intent, the current engine selects Planned Mode; otherwise it selects Fast Mode.
- **`Ambiguity`**: determines whether the request needs clarification before routing/execution.
- **`Negotiation`**: handles interaction with a Planned task that is waiting for approval. It can approve, replan, retrieve memory/task summaries, request limited tools, ask the user, or produce a final response.
- **`Intervention`**: interprets new user guidance received while a task is active. It can update facts/decisions/constraints/open issues and can request replanning for Planned Mode.
- **`AssessTaskRecovery`**: handles later user interaction with a preserved recoverable failed task and decides whether to clarify, retry/continue, replan, or start a new task.

## 2. Acceptance Criteria and Planning

- **`DefineAcceptanceCriteria`**: derives explicit acceptance criteria for a Planned task.
- **`DefineCriteriaDependencies`**: maps dependencies between acceptance criteria.
- **`Plan`**: generates a structured plan draft.
- **`TagTargets`**: annotates plan draft steps with likely target entities/files/symbols.
- **`EvaluatePlan`**: critiques/validates a generated plan before it is accepted.
- **`EvaluateStep`**: decides whether a Planned step is narrow/executable enough or requires decomposition.
- **`DecomposeStep`**: replaces a step with smaller steps when `EvaluateStep` rejects it as too broad.
- **`LinkSteps`**: intelligently rewires plan dependencies when a step is decomposed, rather than naively mapping downstream dependencies to the last decomposed sub-step.
- **`SynthesizeTarget`**: explicitly synthesizes and tags the operational target (e.g., target components or entities) to focus the agent during plan generation.

Plan/replan/decomposition loops are bounded using `max_plan_depth` in the current engine.

## 3. Step Execution

- **`ExecuteStep`**: the central model decision primitive for an active step. It can request a tool, memory retrieval, task summaries, user clarification, or declare the step complete.
- **`FormulateHypothesis`**: generates a hypothesis/expected result before eligible mutating actions. The current engine applies this branch to mutating tools except `execute_process`.
- **`EvaluateStepResult`**: evaluates a tool/result or completion claim and can return statuses including success, partial, verification required, needs more information, plan invalid, or failed.
- **`AssessBlindExecution`**: Dynamic-mode soft decision point invoked when the blind-action threshold is reached while full immediate evaluation is being skipped.
- **`CritiqueApproach`**: critiques the current approach after repeated failed evaluations.
- **`RouteNext`**: selects recovery routing after a failed reflection, including rollback, replan, repair, or asking the user.

`ExecuteStep` does not itself execute the OS/filesystem operation. It requests a tool; the Tool Manager executes it.

## 4. Step Completion and Working Context

- **`GenerateStepArtifact`**: produces the step artifact written under the current task's artifact directory.
- **`ExtractStepResult`**: converts the completed step/extras into a structured result containing summary/facts/decisions used by later state/context.
- **`RefreshTaskContext`**: rebuilds the active structured task context after a normal Planned step completes. It receives the previous task context, new `StepResult`, and acceptance criteria.
- **`PruneMemory`**: allows the agent to explicitly discard stale or obsolete memory artifacts from the active task context to keep it clean.

If `RefreshTaskContext` fails, the engine falls back to appending the extracted facts/decisions deterministically.

## 5. Final Verification and Synthesis

- **`IterativeEvaluation`**: final Planned-task acceptance auditor. It can request only non-mutating tools and iterates up to the planning-depth bound.
- **`SynthesizeTask`**: produces the Fast Mode user-facing result when the synthetic Fast step completes.
- **`SummarizeTask`**: creates the task summary used during memory finalization. If it fails, the engine has a deterministic fallback based on recent step summaries.
- **`ConsolidateContext`**: classifies completed working-memory facts/decisions into semantic facts or architectural decisions suitable for durable memory.

The engine also has a final-response generation call for some flows; that call is recorded as `GenerateFinalResponse` in inference statistics/logging but is not a tool.

## 6. Project Crawling and Memory Extraction

- **`NarrativeFrame`**: generates a structural narrative representation for a source chunk during crawling.
- **`ExtractKeywords`**: extracts index keys from Narrative Frames/task summaries and other memory candidates before embedding/retrieval storage.

The engine also performs durable-memory extraction from user messages and memory retrieval/reranking through its memory pipeline. Those operations should be described according to the current engine/memory implementation rather than assumed to be a generic conversation-compression system.

## 7. Important Non-Primitive Components

Several important operations are not model primitives:

- **Tool Manager**: executes registered file/process/artifact/git tools.
- **Context builder**: deterministically assembles the current prompt from task state, DAG prerequisites, messages, and extras.
- **State store**: persists session/task/tool state.
- **Memory store**: persists project/session memory items and task summaries.
- **Filesystem watcher**: detects changes and marks files stale.
- **Crawler orchestration**: synchronously coordinates indexing/chunking plus Narrative Frame/keyword/embedding generation.

Keeping these distinctions matters when reasoning about Loa's control flow and inference count.
