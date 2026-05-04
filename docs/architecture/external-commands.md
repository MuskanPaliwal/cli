# External Commands

## Overview

The Entire CLI supports kubectl-style external commands — standalone binaries on `$PATH` that extend the CLI without modifying the main repository. When the user invokes `entire <name>` and `<name>` isn't a built-in subcommand, the CLI looks for an `entire-<name>` binary on `$PATH` and execs it with the remaining arguments. Stdio passes through, exit codes propagate, and the parent CLI does no further processing of the child's output.

This is **not** the same mechanism as the [external agent protocol](external-agent-protocol.md). External commands have no protocol, no JSON contract, no lifecycle hooks. Use the agent protocol when you need checkpoint integration; use external commands for everything else.

## Resolution

The CLI does not scan `$PATH` at startup. Resolution is lazy: when `os.Args[1]` doesn't match a built-in subcommand, the CLI calls `exec.LookPath("entire-" + os.Args[1])`. If a binary is found and executable, it runs before Cobra parses arguments.

Rules, in order:

1. **Built-ins win.** If the first argument matches a Cobra subcommand (or one of its aliases), the external command is never considered.
2. **Reserved names are skipped.** Names beginning with `agent-` are reserved for the [agent protocol](external-agent-protocol.md). The resolver refuses to invoke them as external commands.
3. **Path-traversal candidates are rejected.** Names containing `/` or `\` never resolve.
4. **Found-but-not-executable surfaces as a launch error.** If `entire-<name>` exists on `$PATH` but lacks the executable bit, the resolver reports `Failed to run plugin entire-<name>` with exit code 1, rather than falling through to Cobra's "unknown command" path.

## Environment

Each external-command invocation receives:

| Variable | Description |
|---|---|
| `ENTIRE_CLI_VERSION` | The CLI's version string (e.g. `0.42.0`, `dev`) |
| `ENTIRE_REPO_ROOT` | Absolute path to the git repository root, when the CLI is invoked inside one. Omitted otherwise. |

Plus the parent's full environment. The working directory is **not** changed — external commands run in the user's current directory, the same as any other shell command.

## Author Contract

External commands are arbitrary executables. No SDK, no protocol, no manifest. The contract:

- **Stdio is the parent's terminal.** Stdin, stdout, and stderr are connected directly. The command can prompt interactively, stream output, and behave like any other CLI tool.
- **Exit codes propagate verbatim.** The parent `entire` exits with the child's exit code.
- **Signals reach the child.** Terminal signals (Ctrl+C) reach the child directly via the foreground process group. If the parent's context is cancelled (e.g. via `signal.Notify` plumbing), the child receives `SIGINT` with a 5-second grace before the runtime falls back to `SIGKILL`. Commands that need clean shutdown should trap `SIGINT`.
- **Arguments after the command name pass through verbatim.** `entire pgr --help foo` invokes `entire-pgr` with argv `["--help", "foo"]`. Cobra's flag parsing does not run.
- **Windows.** On Windows, `exec.LookPath` resolves `.exe`, `.bat`, and `.cmd` extensions automatically. The "found but not executable" path is Unix-only — Windows treats extension match as the only correctness signal.

## What External Commands Do Not Get

- **No checkpoint integration.** File modifications are not tracked in checkpoints. External commands do not appear in `entire activity`. If a tool needs to participate in the session/checkpoint lifecycle, it must use the [agent protocol](external-agent-protocol.md) instead.
- **No transcript recording.** External-command stdio is not captured.
- **No hook installation.** External commands cannot register git hooks or agent hooks via the resolver. They are free to install their own, but `entire` does not coordinate.
- **No automatic update checks for the command itself.** The CLI runs `versioncheck.CheckAndNotify` for the parent CLI's version, not the child's. Authors should handle their own update notifications.

## Telemetry

External-command invocations are tracked only for names on a hardcoded allowlist (`officialPlugins` in `cmd/entire/cli/plugin_official.go`). Third-party command names are **never** sent — even with telemetry opted in. The reasoning matches gh's extension-telemetry posture: arbitrary command names can carry sensitive identifiers (project names, vendor names), and the safest default is silence.

When an allowlisted command runs successfully, the CLI emits a `cli_plugin_executed` event with:

- `plugin` — the command name
- `command` — `entire <name>`
- `cli_version`, `os`, `arch`, `isEntireEnabled`

Args and flags are deliberately **not** recorded.

Telemetry fires only when:

1. The command name is in `officialPlugins`.
2. `entire` settings have `Telemetry: true`.
3. `ENTIRE_TELEMETRY_OPTOUT` is unset.
4. The command exited with status 0. Failed/crashing invocations are not tracked, matching Cobra's `PersistentPostRun` semantics for built-in commands.

## Adding an Entire-Shipped Command to the Allowlist

When publishing an Entire-owned external command (e.g. `entire-pgr`):

1. Append the command name to `officialPlugins` in `cmd/entire/cli/plugin_official.go`.
2. Match must be exact and case-sensitive — the binary on disk is `entire-<name>`.
3. Update or add tests if the command has unusual telemetry shape.

Once allowlisted, `cli_plugin_executed` events for that command will flow through the existing PostHog pipeline.

## Comparison with the Agent Protocol

| | External Commands | [Agent Protocol](external-agent-protocol.md) |
|---|---|---|
| **Binary name pattern** | `entire-<name>` | `entire-agent-<name>` |
| **Discovery** | Lazy, on first non-built-in arg | Lazy at command entry, gated by `external_agents` setting (setup flows bypass the gate via `DiscoverAndRegisterAlways`) |
| **Communication** | Process exec; stdio passthrough | Subcommand protocol; JSON over stdin/stdout |
| **Versioning** | None | `ENTIRE_PROTOCOL_VERSION` envelope |
| **Lifecycle integration** | None | Full (sessions, checkpoints, hooks, transcripts) |
| **Telemetry** | Allowlist only | Standard agent telemetry |
| **Working directory** | User's cwd | Repository root |
| **Use when** | You want to add a CLI verb | You want an AI agent to participate in checkpointed sessions |

## Implementation

The resolver lives in `cmd/entire/cli/plugin.go`. The entry point is `MaybeRunPlugin(ctx, rootCmd, args)`, called from `cmd/entire/main.go` before `rootCmd.ExecuteContext`. Returns `(handled bool, exitCode int)` — when `handled` is true, the caller exits with `exitCode`; otherwise it falls through to normal Cobra execution.

Key files:

- `cmd/entire/cli/plugin.go` — entry point, `resolvePlugin`, `runPlugin`
- `cmd/entire/cli/plugin_official.go` — `officialPlugins` allowlist, `IsOfficialPlugin`
- `cmd/entire/cli/telemetry/detached.go` — `BuildPluginEventPayload`, `TrackPluginDetached`
- `cmd/entire/cli/integration_test/external_command_test.go` — end-to-end coverage of the resolution path
