# AGENTS.md

This file is for coding agents working in this repository.
It captures the repository-specific workflow and the style conventions already present in the code.

## Scope and stack
- Language: Go
- Module: `vpngate`; declared Go version: `go 1.26.1`
- App type: small HTTP app with embedded HTML templates
- Main entrypoint: `main.go`
- Core packages: `internal/web`, `internal/vpngate`
- Templates: `internal/web/templates/*.html`
- Tooling: Go modules only; no Node, Python, or Make-based workflow detected

## Additional rule files
- No `.cursor/rules/`, `.cursorrules`, `.github/copilot-instructions.md`, or prior root `AGENTS.md` were present when this file was created.
- If any appear later, treat them as additional repo-specific instructions and merge them into your plan before editing.

## Directory map
- `main.go`: startup, logger, initial refresh, graceful shutdown
- `internal/web/app.go`: routes, refresh flow, filtering, sorting, formatting, shared state
- `internal/vpngate/iphone.go`: VPN Gate fetch/parsing logic and typed domain model
- `internal/vpngate/iphone_test.go`: unit tests
- `internal/vpngate/iphone_live_test.go`: opt-in live integration test
- `internal/vpngate/doc.go`: package doc comment

## Canonical commands
- Important: the commands in this section are reference commands. Unless the user explicitly asks for execution, agents should not proactively run the project, tests, builds, or verification commands.
- Run app: `go run .`
- Run on another port: `PORT=8081 go run .`
- Build app: `go build .`
- Build all packages: `go build ./...`
- Format touched files: `gofmt -w <file1.go> <file2.go>`
- Check formatting: `gofmt -l .` (should print nothing)
- Static analysis: `go vet ./...`
- Run all tests: `go test ./...`
- Run one package: `go test ./internal/vpngate`
- Run one test function: `go test ./internal/vpngate -run '^TestParseIPhoneResponse$'`
- Run one subtest: `go test ./internal/vpngate -run 'TestParseIPhoneResponse/valid response'`
- Run without cache: `go test ./... -count=1`
- Run the live test only: `VPNGATE_LIVE_TEST=1 go test ./internal/vpngate -run '^TestFetchIPhoneServersLive$' -count=1`
- Suggested validation command when the user explicitly asks for verification: `go test ./... && go vet ./...`

## Working style for agents
- Read the affected package and nearby tests before editing.
- Keep changes local; prefer the smallest surface area that solves the task.
- For code changes, run `gofmt -w` on touched Go files.
- Important: do not run the project for testing by yourself. Unless the user explicitly asks, do not execute `go run .`, browser/manual smoke tests, `go test`, `go vet`, or `go build` as a default validation step.
- If verification would help, provide the exact commands the user can run, or ask for permission before executing them.
- For docs-only changes, avoid command execution unless the user explicitly requests it.

## Code style guidelines

### Formatting and imports
- Use `gofmt`; do not hand-align whitespace.
- Keep imports in standard Go groups: stdlib first, blank line, then module imports such as `vpngate/internal/web`.
- Do not leave unused imports.
- Avoid alias imports unless needed for disambiguation.

### Package and file organization
- Keep package names short, lowercase, and descriptive.
- Preserve current ownership: `internal/vpngate` handles upstream API/domain parsing; `internal/web` handles HTTP and presentation logic.
- Prefer extending an existing package over creating a new package for small features.
- Add package doc comments when introducing new packages.

### Naming
- Exported identifiers use PascalCase; unexported helpers use camelCase.
- Preserve initialism casing already used here: `IP`, `HTTP`, `URL`, `VPN`.
- Follow existing domain terminology rather than renaming things for style alone.
- Prefer descriptive helper names like `formatDurationCN`, `safeText`, `sortServers`, `validateHeader`.

### Types and API design
- Prefer concrete structs over `map[string]any` or other untyped containers.
- Keep parsed domain data strongly typed as early as possible.
- Match existing numeric choices: `int64` for traffic/speed/score/session/user counters, `int` where the code already uses it for ping.
- Put `context.Context` first in request/network-bound functions.
- Keep dependency injection patterns consistent: accept `*http.Client` or `*log.Logger` when the package already does so.
- Only allow `nil` dependencies where the existing code already defines a sensible default.

### Error handling
- Wrap underlying errors with `fmt.Errorf(...: %w)` when propagating them.
- Include concrete context such as field name, row number, or operation name.
- Return early on bad input or invalid state.
- In HTTP handlers, validate method/path first and fail fast.
- Keep user-facing error text in Chinese to match the rest of the app.
- Ignore errors only for clearly best-effort operations.

### Logging
- Use the package logger already being passed around or stored on the struct.
- Keep operational logs concise and Chinese-language, matching the existing style.
- Prefer `Printf`/`Println` with actionable context.
- Reserve fatal exits for startup/shutdown failures in `main.go`.
- Do not add noisy debug logging unless requested.

### HTTP handlers and templates
- Stick to the standard library HTTP stack (`net/http`, `http.NewServeMux`).
- Check method and path before doing expensive work.
- Set `Content-Type` explicitly for HTML and JSON responses.
- Use `context.WithTimeout` around request-triggered outbound work.
- Prefer POST for state-changing endpoints like refresh.
- Follow the existing POST-redirect-GET flow after refresh actions.
- Keep templates embedded via `embed` and rendered with `html/template`.
- Preserve server-rendered behavior unless a larger change is explicitly requested.

### Concurrency and shared state
- Protect mutable shared state with `sync.RWMutex`.
- Keep lock scope tight.
- Do not hold locks while doing network I/O, template work, or other slow operations.
- Copy slices/maps before building derived views from shared state when needed.
- Follow the existing read-lock / copy / unlock approach in `buildPageData`.

### Strings, UX copy, and localization
- Preserve the existing Chinese UI and operational copy.
- Do not translate existing Chinese strings into English as part of routine refactors.
- Keep punctuation and status wording consistent with the current UI.
- Prefer shared helpers for fallback text/formatting over duplicating inline string logic.

### Parsing and validation
- Prefer named constants for protocol markers, URLs, limits, and headers.
- Validate upstream response structure before mapping fields into structs.
- Keep parsing helpers small and single-purpose.
- Fail with specific errors when headers or field counts are wrong.
- Preserve response size limits before parsing remote payloads.

### Tests
- Follow the existing Go testing style: table-driven tests, `t.Run(...)`, `t.Fatal`/`t.Fatalf`.
- Use substring assertions for wrapped error messages when exact text may vary.
- Keep unit tests deterministic and self-contained.
- Gate live/network tests behind an explicit env var and `t.Skip` by default.
- Add or update tests when changing parsing, filtering, sorting, or error behavior.

### Dependency policy
- Prefer the Go standard library when it is sufficient.
- Do not add third-party routing, logging, or templating libraries without clear need.
- Do not introduce Node/Python tooling unless explicitly asked.

## Guardrails
- Do not change the declared Go version in `go.mod` unless explicitly requested.
- Do not start the app or run the project for testing on your own unless the user explicitly asks.
- Do not replace the standard library HTTP stack with a framework without approval.
- Do not convert simple typed structs/helpers into generic abstractions without clear payoff.
- Do not enable the live test in default validation flows unless the task explicitly needs network coverage.

## Verification checklist
Use this checklist only when the user explicitly requests command execution.
- Touched Go files are formatted with `gofmt`.
- `gofmt -l .` is empty.
- Relevant single tests pass when you changed targeted behavior.
- `go test ./...` passes for code changes.
- `go vet ./...` passes for logic-heavy changes.
- New behavior and copy stay consistent with the existing Chinese UX/logging style.
