# Repository Guidelines

## Project Structure & Module Organization

Xhunter is a Go 1.26 agent runtime with **no third-party runtime dependencies**. It ships **public, importable packages** — `llm/` (the neutral model contract), `harness/` (the neutral model-calling loop), the capability base packages `workspace/` / `git/` / `ext/`, the domain package `hunt/`, `provider/...` (protocol implementations), `providerconfig/` (the environment-variable contract for model access), `prompt/...` (default prompt plugins) — plus the CLI in `cmd/xhunter`; packages that exist only for the CLI live in `internal/`:

- `llm/` — the neutral contract: message/tool-call/event shapes plus the `Provider`/`Session`/`Caps` interfaces, and the `ToolDecl` schema helpers (`ObjectSchema`/`ObjectSchemaAnyOf`). No orchestration, no tool semantics. Everything else depends on it; protocol packages depend only on it and on `provider/adapter`.
- `harness/` — the **neutral model-calling loop**, and nothing else. It is two files: `Engine`, which drives a fixed cycle (`Prepare` → [infer → receive → `OnTurn`] → `Finalize`), plus `Run`/`Turn` and the terminal value types. It knows **no business model** — no task, path, selector, gate, checkpoint, or git concept — and it owns **no collaborators**: no context builder, session recorder, event sink, or tool executor. Business enters through exactly **three handler groups** — `PrepareHandler(ctx, *Run) error`, `OnTurnHandler(ctx, *Run, *Turn) (bool, error)`, `FinalHandler(ctx, *Run) error` — and `New(provider, prepare, onTurn, final)` is the only constructor (a missing provider is an assembly error). The loop only collects `turn.Text`/`turn.Calls` from the stream; **executing those calls is the `OnTurn` handler's job**, and the same handler assembles the next turn's messages. It depends only on `llm`; swapping the handler set is swapping the business.
- `workspace/` — the file-I/O **capability base package**: `Workspace` (read/stat/list) and `Storage` (plus the single `WriteRange` primitive), with `LineRange`/`ByteRange`/`FileEdit`/`FileContent` coordinate types. It defines the abstraction, not the implementation; `internal/workspace/osfs` is the local-filesystem implementation.
- `git/` — the version-control **capability base package**: `GitWorktree` (baseline, task branch, commit, diff, cleanup) plus `RepoRef`/`Commit`. `internal/git/cli` is the command-line implementation (entry points in place; implementations pending).
- `ext/` — the symbol-resolution **capability base package**: `ExtHost` (capabilities, locate, close) plus `ExtCaps`/`Prepared`/`Impact`/`Precision`/`LocateRequest`. No implementation yet.
- `hunt/` — the **code-editing-agent domain**: task inputs (`Bounty`/`Repo`/`Budget`/`Checkpoint`), the tool-call shape (`Call`/`Selector`/`Result`/`WriteOp`), the `Gate` quality gate, the `Policy`/`Primitive`/`PromptPlugin` contracts, and the default executor `Session` — it supplies the loop's **three handler groups**, executes the tool calls at the turn boundary, and is also the `Facts` its primitives see. Its own collaborators (`ContextBuilder`, `SessionRecorder`, `EventSink`, plus `ExternalEvent`/`Phase`) live in `hunt/runtime.go`, and `harness` does not know them. Two extension points sit on that boundary: the `ResultFilter` chain (runs after execution and **before** history is recorded, so filtered text actually reaches the model) and primitive decorators (intercept / re-aim / beautify a single call, still writing only through the `Committer`). It depends on `harness` and the three base packages; `harness` does **not** know `hunt`.
- `hunt/basic/`, `hunt/symbolic/`, `hunt/gate/` — **primitives grouped by domain, one package per domain**: `basic` (read/write/edit/find/glob — text addressing only), `symbolic` (symbol read/edit/rename — depends on `basic` + `ext`, locates to a byte range then reuses basic read/write; symbolization and degradation live entirely here), `gate` (the `check` primitive + automatic-checkpoint hook). Each exports `hunt.Primitive` constructors; `symbolic`/`gate` are declare-only for now. The assembly layer (`cmd/xhunter/primitives.go`) decides which exist and in what order, and hands `hunt.Session` a `ToolFactory` it calls once the workspace is open.
- `provider/adapter/` — the pieces every protocol implementation shares: minimal SSE framing, position-keyed call assembly, error shapes, and the context-window contract. Contains no concrete protocol and no assembly concept.
- `provider/<protocol>/` — one protocol per package (`openaichat`, `openairesponses`, `anthropicmessages`). Each owns its wire types and stream framing **by hand — do not add vendor SDKs**; each package doc opens with a "protocol version baseline" (endpoint, version header, shapes, terminal semantics, when this package must change).
- `providerconfig/` — the **environment-variable contract for model access**: it owns the variable names, the parsing/validation rules, and `Resolved`; consumed by the assembly layer. There are no configuration files and no model catalogs.
- `prompt/<kind>/` — **default prompt plugins**: implementations of `hunt.PromptPlugin` that build the first-turn `system`/`user` text (`agentsmd` for project conventions, `skills` for the skill manifest). Plain constructors (`New(ws workspace.Workspace)`) over one small public contract, no registration. The workspace is an explicit constructor parameter (a runtime product); the assembly layer hands `hunt.Session` a `PromptPluginFactory` per stage, and the plugin order **is** the concatenation order.
- `cmd/xhunter/` — CLI **and the assembly layer**: builds the primitive set, the prompt plugins, the context/session/event collaborators and the backend implementations, stitches them into `hunt.Session`, hands the loop `harness.New(provider, session.Prepare, session.OnTurn, session.Finalize)`, maps a protocol value to a protocol factory, and turns environment variables into request headers (endpoints are never preset; auth shape comes from env vars).
- `internal/workspace/osfs/` — the local-filesystem `workspace` implementation: directory-rooted, relative paths only, path-boundary and symlink-escape checks, and root validation at open time (a bad root fails in the prepare phase, before the first inference).
- `internal/git/cli/` — the command-line `git` implementation.
- `internal/policy/` — the strategy engine (the default `hunt.Policy`): default-deny, path boundaries (`.xhunter/**` write protection with the `skills.draft` exception), and the token/turn/wall-clock budgets.
- `docs/` — four governing documents plus one memo: `docs/xhunter-product-design.md`（产品边界与 FR/NFR/AC）、`docs/xhunter-usage.md`（外部契约与使用方式，含事件流 SSOT）、`docs/xhunter-architecture.md`（分层、组件、控制流时序、不变量 INV、接口验收 IA）、`docs/xhunter-status.md`（实现现状与排期，状态 SSOT）。`docs/xhunter-memo-multi-agent-route.md` 是**备忘录、非规范**（备查未采纳的路线与缺口清单）；规范一律以那四份为准。
- `scripts/check.py` — local build, vet, test, and target-platform checks.

## Build, Test, and Development Commands

Run the canonical validation suite after changes:

```bash
python scripts/check.py
```

This runs `go build ./...`, `go vet ./...`, `go test ./... -count=1`, and cross-compiles for `linux/amd64` when the host differs. Use `python scripts/check.py --probe` to inspect the toolchain only. **The target platform is `linux/amd64`** — a change is not done until it has been validated there, not only on the development host. Direct equivalents are `go build ./...`, `go vet ./...`, and `go test ./...`.

## Coding Style & Naming Conventions

Use idiomatic Go and `gofmt` formatting. Keep packages focused; prefix unexported identifiers and define public contracts as interfaces with concise contract comments. Name files after their domain (`engine.go`, `catalog.go`) and tests with Go's `*_test.go` convention. **Comments must be self-contained**: explain the reasoning inline rather than citing document IDs — no `FR-x.y` / `INV-n` / section references, so the code stays readable without the docs.

## Testing Guidelines

Write table-driven or stub-based Go tests beside the package under test. Design tests to run without network access or a real model provider; inject fakes through existing ports. Every test should trace to an acceptance criterion or prevent a specific regression. Run all tests through `scripts/check.py`; do not treat compilation alone as validation.

## Task, Commit, and Pull Request Guidelines

Task decomposition is currently being rebuilt around the task lifecycle architecture (see `docs/xhunter-architecture.md`); until then, treat each focused change as one commit with an imperative subject. Before implementing a new contract or acceptance item, update the matching document under `docs/`. Pull requests should state the task or issue, summarize behavior and failure paths, list validation performed, and include linked documents or screenshots for CLI-visible changes.

**Document numbering**: IDs are the cross-document key — `FR-`/`NFR-`/`AC-` in `xhunter-product-design.md`, `INV-`/`IA-` in `xhunter-architecture.md`, plus `L-`/`H-` step and component labels in the status document — and they are not renumbered. A **lettered sub-ID must have its own definition row** (`| FR-12.2c | … |`): naming one only inside another item's prose leaves every reference pointing at nothing, and the reference reads as if the requirement were missing rather than as if it were described elsewhere.

## Security & Configuration Tips

Runtime must not add network calls or dependencies without an approved task decision. **Protocol packages in particular must not pull in vendor SDKs**: the transport layer is written by hand so that it only changes when the protocol changes, and so the core binary stays dependency-free. Keep provider secrets out of source: model-access facts come from environment variables only (`XHUNTER_API_KEY`, or `{env:VAR}` references inside `XHUNTER_HEADERS`), and there are no configuration files or model catalogs to leak them into.
