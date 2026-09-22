# Repository Guidelines

## Project Structure & Module Organization

Xhunter is a Go 1.26 harness with **no third-party runtime dependencies**. It ships **public, importable packages** — `llm/` (the neutral conversation/model contract), `harness/` (the reusable executor core), `provider/...` (protocol implementations and their shared pieces), `providerconfig/` (provider configuration contract), `prompt/...` (default prompt plugins) — plus the CLI in `cmd/xhunter`; packages that exist only for the CLI live in `internal/`:

- `llm/` — the neutral contract: message/tool-call/event shapes plus the `Provider`/`Session`/`Caps` interfaces. No orchestration, no tool semantics. `harness` depends on it; protocol packages depend only on it and on `provider/adapter`.
- `harness/` — stage executor: `Engine` drives the pre/turn/post chains, `Context` holds per-turn state while `Hunt` holds task-level state, `Runtime` runs the tool-call pipeline (inspect → dispatch → locate → decide → plan → commit); `binding.go` is the only place where model-side calls are translated into this project's domain calls. **It knows no concrete primitive**: which tools exist and in what order comes in as data (`NewRuntime(policy, ext, tools)`), and dispatch keys off each tool's addressing kind rather than its name. **Must not depend on `cmd/` or `internal/`.**
- `provider/adapter/` — the pieces every protocol implementation shares: minimal SSE framing, position-keyed call assembly, error shapes, and the context-window contract. Contains no concrete protocol and no assembly concept.
- `provider/<protocol>/` — one protocol per package (`openaichat`, `openairesponses`, `anthropicmessages`). Each owns its wire types and stream framing **by hand — do not add vendor SDKs**; each package doc opens with a "protocol version baseline" (endpoint, version header, shapes, terminal semantics, when this package must change).
- `providerconfig/` — provider/model configuration model, validation, and resolution; consumed by the assembly layer.
- `prompt/<kind>/` — **default prompt plugins**: implementations of `harness.PromptPlugin` that build the first-turn `system`/`user` text (`agentsmd` for project conventions, `skills` for the skill manifest). Same shape as protocol packages — plain constructors over one small public contract, no registration, no knowledge of the rest of the pipeline. The assembly layer picks which ones run and in what order, and that order **is** the order the text is concatenated in.
- `cmd/xhunter/` — CLI **and the assembly layer**: maps an SDK value to a protocol factory, and turns configuration into request headers (endpoints are never preset; auth shape comes from config).
- `internal/workspace/osfs/` — the file-operation backend: a directory-rooted workspace (read-only view + the single write primitive), plus path-boundary and symlink-escape checks. The kernel only defines the contract (`Workspace`/`Storage`); the assembly layer injects this implementation through `Deps.Workspaces`.
- `internal/git/cli/` — the git backend (command-line based): baseline, task branch, commits, diff, cleanup. Entry points are in place; implementations pending.
- `internal/modelcatalog/` — model catalog conversion, storage, and refresh (CLI-side only).
- `internal/primitives/<name>/` — **the operation primitives, one package each** (`read`, `write`, `edit`, `find`, `rename`, `glob`, `check`). Each exports a single `Tool()` giving name + model-visible declaration + implementation + addressing kind. The framework knows none of them: the assembly layer (`cmd/xhunter/primitives.go`) decides which exist and in what order.
- `docs/` — three governing documents: `docs/xhunter-product-design.md`（产品边界与 FR/NFR/AC）、`docs/xhunter-usage.md`（外部契约与使用方式，含事件流 SSOT）、`docs/xhunter-architecture.md`（分层、组件 H1~H7、控制流时序 G/R/D、不变量 INV、接口验收 IA）。
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

Task decomposition is currently being rebuilt around the task lifecycle architecture (see `docs/xhunter-architecture.md` §6); until then, treat each focused change as one commit with an imperative subject. Before implementing a new contract or acceptance item, update the matching document under `docs/`. Pull requests should state the task or issue, summarize behavior and failure paths, list validation performed, and include linked documents or screenshots for CLI-visible changes.

## Security & Configuration Tips

Runtime must not add network calls or dependencies without an approved task decision. **Protocol packages in particular must not pull in vendor SDKs**: the transport layer is written by hand so that it only changes when the protocol changes, and so the core binary stays dependency-free. Keep provider secrets out of source and snapshots (configuration carries `{env:VAR}` references only); configuration and model catalogs are auditable data, not test fixtures.
