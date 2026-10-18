<div align="center">
  <img src="loa.png" alt="Loa Logo" width="400"/>
  <h1>Loa</h1>
  <p><strong>The Autonomous Local Coding Agent</strong></p>
  <p>
    <a href="#core-characteristics">Core Characteristics</a> •
    <a href="#quick-start">Quick Start</a> •
    <a href="#documentation">Documentation</a> •
    <a href="#faq">FAQ</a> •
    <a href="#contributing">Contributing</a> •
    <a href="LICENSE">License</a>
  </p>
</div>

<br/>

## Summary

Loa is a local-first, stateful coding agent intended for project-wide engineering tasks and long-running autonomous work. Its control layer is built around explicit state transitions, structured tool use, persistent task state, memory retrieval, evaluation, and recovery rather than one open-ended model/tool loop.

For complex work, Loa uses a mutable Directed Acyclic Graph (DAG). The graph is both an execution plan and part of the context topology: a step's dependencies determine which completed step results are injected as direct prerequisites for that step. The plan can be decomposed or replanned as work progresses. See [Architecture & Pipeline](docs/core_concepts/architecture.md) and [Design Philosophy](docs/core_concepts/design_philosophy.md) for the implementation model.

Loa stores project/session state locally and can run either directly on the host or inside the optional Docker sandbox. Model inference is sent to the OpenAI-compatible endpoint you configure; embeddings can either run locally on CPU or use the configured external embedding endpoint.

> [!IMPORTANT]
> **Do not use Loa with a metered cloud API unless you explicitly understand and accept the potential cost and rate-limit impact.**
>
> Loa is designed around local or otherwise self-hosted inference where a high number of model calls is operationally acceptable. Model calls occur throughout the system, not only when code is written. Project crawling generates a Narrative Frame and keyword extraction for unindexed code chunks. Fast Mode is lighter than Planned Mode but still uses Loa's inference, retrieval, execution, and memory machinery. Planned Mode can invoke the model repeatedly for criteria, planning, execution decisions, hypotheses, evaluation, recovery, context refresh, final verification, and task-memory finalization.
>
> OpenAI-compatible cloud endpoints are technically usable, but they are not the intended deployment model. Even project setup/indexing can consume a substantial number of requests, and long Planned Mode tasks can consume very large numbers of model calls.

## Core Characteristics

1. **Mutable DAG execution for planned tasks.** Planned Mode creates explicit acceptance criteria and a DAG plan before execution. Completed dependencies are routed into dependent steps as prerequisite results, and the remaining plan can be decomposed or replanned when needed.
2. **Explicit execution and evaluation states.** Tool execution is separated from model decisions. Mutating actions can be preceded by a hypothesis, results can be evaluated independently, and failures can route into repair, rollback, clarification, or replanning.
3. **Local state and project memory.** Runtime state is stored under `.loa/`; project configuration is stored in `.loa.json`. Project structural memory is built from Narrative Frames anchored to indexed project files. Session memory and task summaries are stored separately per session.
4. **Reconstructed context instead of continuously accumulating history.** Every inference receives a newly assembled context containing the active task state, current step, direct prerequisite results, recent breadcrumbs/messages, and selected retrieved/tool context within configured budgets. Loa does not use recursive whole-context compression as its context-window strategy. See [Memory System](docs/core_concepts/memory_system.md).
5. **Optional Docker execution boundary.** The sandbox isolates the container operating system from the host while deliberately mounting the target project read/write. File tools are project-root constrained, while shell/process tools execute with the privileges and filesystem visibility of the environment in which Loa itself is running. See [Sandbox and Security](docs/core_concepts/sandbox_and_security.md).
6. **Observable state.** The UI exposes the current plan/graph, working/session/project memory views, artifacts, the last composed model context, execution logs, approvals, uploads, and task controls.

## Quick Start

### Prerequisites

- Git
- A local or self-hosted OpenAI-compatible inference endpoint. Ollama and similar servers can be used as long as the endpoints Loa expects are available.
- Docker and Docker Compose if you want to use `loa-sandbox`.

The provided installer and release artifacts currently target Linux `amd64` and `arm64` environments.

### Installation

The setup script installs Loa under `~/.loa/bin`, installs/downloads `ast-grep`, ensures `ctags` is available where supported, optionally installs the sandbox files, and optionally downloads a local GGUF embedding model. If `~/.local/bin` or `~/bin` already exists and is writable, the installer symlinks `loa` there; otherwise it prints the installed binary path so you can add it to `PATH` yourself.

```bash
curl -fsSL https://raw.githubusercontent.com/laughingmandev/loa/main/scripts/setup.sh | bash
```

The installer is plain shell and can be inspected directly at [`scripts/setup.sh`](scripts/setup.sh) before running it.

**Build from source**

```bash
git clone https://github.com/laughingmandev/loa.git
cd loa
bash scripts/setup.sh --local
```

The local setup path builds Loa with the `localembed` tag so the local CPU embedding backend is available.

Before starting Loa for the first time, read the [Usage Guide](docs/getting_started/usage_guide.md). Initial project setup performs a project scan and can itself generate many model calls because the crawler creates semantic Narrative Frames for unindexed source chunks.

### Running Loa

#### Sandboxed mode

If you selected sandbox installation during setup, add the alias printed by the installer and start it from the project you want Loa to work on:

```bash
cd /path/to/your/project
loa-sandbox
```

The sandbox publishes the Loa UI on port `8104` and mounts the current project at `/workspace` read/write.

The current sandbox launcher uses an already-installed compatible Linux ELF Loa binary when available. Its source-build fallback expects a developer source checkout at a specific local path, so the current sandbox launcher should not be treated as a general macOS/Windows bootstrap mechanism.

#### Host mode

Host mode runs Loa directly with your user privileges:

```bash
cd /path/to/your/project
loa .
```

The CLI default is `127.0.0.1:7171`, so the UI is then available at `http://127.0.0.1:7171`.

To bind a different address explicitly:

```bash
loa -addr 0.0.0.0:8104 .
```

> [!CAUTION]
> In Host Mode, `execute_process` and `execute_shell` run directly on the host with your user privileges. Shell/process commands are intentionally not safety-parsed and can escape the project root. Use the permission controls accordingly; `ask_selected` with process/shell approval enabled is the default configuration.

## Documentation

### Getting Started

- [Usage Guide](docs/getting_started/usage_guide.md): first-time setup, UI, execution modes, steering, pause/resume, and operational guidance.
- [Settings Guide](docs/getting_started/settings_reference.md): the five settings tabs as they exist in the current UI.

### Core Concepts

- [Design Philosophy](docs/core_concepts/design_philosophy.md): the control-system principles behind Loa.
- [Architecture & Pipeline](docs/core_concepts/architecture.md): current Fast/Planned execution flow, DAG mutation, evaluation, recovery, and reconciliation.
- [Memory System](docs/core_concepts/memory_system.md): project/session memory, Narrative Frames, context reconstruction, and task-memory finalization.
- [Sandbox & Security](docs/core_concepts/sandbox_and_security.md): Docker mounts, file-tool boundaries, process execution, permissions, and attachment resilience.

### Reference

- [Primitives Reference](docs/reference/primitives.md): current model-backed primitives used by the engine and crawler.
- [Tooling Reference](docs/reference/tools.md): registered tools and their actual execution boundaries.
- [Settings Reference](docs/reference/settings.md): configuration fields and defaults. Project overrides are stored in `.loa.json`.

## Future Enhancement Plans

The following is a living list of project ideas rather than a strict roadmap. Items may be changed, reprioritized, or dropped.

- **Multi-language UI & Chat Support:** an auto-translation layer for the web interface while keeping the internal control prompts in their expected language.
- **Expanded Architecture Support:** broader tested host/platform support beyond the current Linux-oriented release/setup path.
- **UI Enhancements:** continued cleanup and refinement of the beta web interface.

## FAQ

**Q: Why did you build Loa?**  
A: I wanted a local-first coding agent whose control state stays observable and steerable during long tasks, and whose planning is allowed to change when execution discovers new information. The mutable DAG and the surrounding state machine grew out of my work on dynamic thought composition rather than from trying to reproduce a conventional chat/tool loop.

**Q: Why does Loa use so many inferences?**  
A: High inference count is an intentional operating characteristic. Loa separates operations that many agents collapse into one prompt: project framing, retrieval, planning, execution decisions, hypotheses, evaluation, recovery, context maintenance, and final verification can all be distinct calls. This is why local/self-hosted inference is the intended deployment model.

**Q: Which local LLM works best with Loa?**  
A: My current practical recommendation is a model around the 27B-32B range, including Qwen variants in that class. Larger models may perform better, but this recommendation is based on the hardware and models I have personally been able to test rather than an exhaustive benchmark.

**Q: What does "OpenAI-compatible" mean here?**  
A: Loa's current client expects model listing at `/v1/models`, chat completions at `/v1/chat/completions`, and external embeddings at `/v1/embeddings`. A Bearer token can be configured when the endpoint requires one.

**Q: What is the difference between Fast and Planned mode?**  
A: Fast Mode skips formal acceptance-criteria generation and the initial multi-step DAG planning/approval path. It still executes inside Loa's stateful tool/retrieval/memory system and still performs task-memory finalization. Planned Mode builds acceptance criteria and a mutable DAG, evaluates/decomposes steps, and runs a final acceptance audit before synthesis.

**Q: Does Loa keep project knowledge only in one RAG database?**  
A: No. Loa uses separate persisted project and session memory stores plus active task context and persisted task/tool state. Project structural memory is anchored to source files and generated by the crawler. Retrieval uses embeddings and reranking, but Loa's memory/context system is not just one global conversation-summary bucket.

**Q: Does Loa support external files?**  
A: Yes. The current UI has session uploads and attachments. Uploaded files can be attached to active prompt context, with read-only or writable behavior handled through the attachment workspace described in the security documentation.

**Q: What is the contribution policy?**  
A: Please open an issue before implementing a feature. Loa has a deliberately narrow target scope, so feature direction should be agreed before significant code is written.

## Contributing

Loa is actively developed. Before submitting a PR, read the architecture documentation and open an issue to discuss feature scope first.
