# Plugin Dispatch

## Overview

The Entire CLI supports kubectl-style plugins — standalone binaries on `$PATH` that extend the CLI without modifying the main repository. When the user invokes `entire <name>` and `<name>` isn't a built-in subcommand, the CLI looks for an `entire-<name>` binary on `$PATH` and execs it with the remaining arguments. Stdio passes through, exit codes propagate, and the parent CLI does no further processing of the plugin's output.

This is **not** the same mechanism as the [external agent protocol](external-agent-protocol.md). Plugins have no protocol, no JSON contract, no lifecycle hooks. Use the agent protocol when you need checkpoint integration; use plugins for everything else.

## Discovery

The CLI does not scan `$PATH` at startup. Resolution is lazy: when `os.Args[1]` doesn't match a built-in subcommand, the CLI calls `exec.LookPath("entire-" + os.Args[1])`. If a binary is found and executable, dispatch happens before Cobra parses arguments.

Resolution rules, in order:

1. **Built-ins win.** If the first argument matches a Cobra subcommand (or one of its aliases), the plugin is never considered.
2. **Reserved names are skipped.** Names beginning with `agent-` are reserved for the [agent protocol](external-agent-protocol.md). The dispatcher refuses to invoke them as plugins.
3. **Path-traversal candidates are rejected.** Names containing `/` or `\` never resolve.
4. **Found-but-not-executable surfaces as a launch error.** If `entire-<name>` exists on `$PATH` but lacks the executable bit, the dispatcher reports `Failed to run plugin entire-<name>` with exit code 1, rather than falling through to Cobra's "unknown command" path.

## Environment

Each plugin invocation receives:

| Variable | Description |
|---|---|
| `ENTIRE_CLI_VERSION` | The CLI's version string (e.g. `0.42.0`, `dev`) |
| `ENTIRE_REPO_ROOT` | Absolute path to the git repository root, when the CLI is invoked inside one. Omitted otherwise. |

Plus the parent's full environment. The working directory is **not** changed — plugins run in the user's current directory, the same as any other shell command.

## Plugin Author Contract

Plugins are arbitrary executables. No SDK, no protocol, no manifest. The contract:

- **Stdio is the parent's terminal.** Stdin, stdout, and stderr are connected directly. Plugins can prompt interactively, stream output, and behave like any other CLI tool.
- **Exit codes propagate verbatim.** The parent `entire` exits with the plugin's exit code.
- **Signals reach the plugin.** Terminal signals (Ctrl+C) reach the plugin directly via the foreground process group. If the parent's context is cancelled (e.g. via `signal.Notify` plumbing), the plugin receives `SIGINT` with a 5-second grace before the runtime falls back to `SIGKILL`. Plugins that need clean shutdown should trap `SIGINT`.
- **Arguments after the plugin name pass through verbatim.** `entire pgr --help foo` invokes `entire-pgr` with argv `["--help", "foo"]`. Cobra's flag parsing does not run.
- **Windows.** On Windows, `exec.LookPath` resolves `.exe`, `.bat`, and `.cmd` extensions automatically. The "found but not executable" path is Unix-only — Windows treats extension match as the only correctness signal.

## What Plugins Do Not Get

- **No checkpoint integration.** Plugin file modifications are not tracked in checkpoints. Plugins do not appear in `entire activity`. If a plugin needs to participate in the session/checkpoint lifecycle, it must use the [agent protocol](external-agent-protocol.md) instead.
- **No transcript recording.** Plugin stdio is not captured.
- **No hook installation.** Plugins cannot register git hooks or agent hooks via the dispatcher. They are free to install their own, but `entire` does not coordinate.
- **No automatic update checks for the plugin itself.** The CLI runs `versioncheck.CheckAndNotify` for the parent CLI's version, not the plugin's. Plugin authors should handle their own update notifications.

## Telemetry

Plugin invocations are tracked only for plugins on a hardcoded allowlist (`officialPlugins` in `cmd/entire/cli/plugin_official.go`). Third-party plugin names are **never** sent — even with telemetry opted in. The reasoning matches gh's extension-telemetry posture: arbitrary plugin names can carry sensitive identifiers (project names, vendor names), and the safest default is silence.

When an allowlisted plugin runs successfully, the CLI emits a `cli_plugin_executed` event with:

- `plugin` — the plugin name
- `command` — `entire <plugin>`
- `cli_version`, `os`, `arch`, `isEntireEnabled`

Plugin args and flags are deliberately **not** recorded.

Telemetry fires only when:

1. The plugin name is in `officialPlugins`.
2. `entire` settings have `Telemetry: true`.
3. `ENTIRE_TELEMETRY_OPTOUT` is unset.
4. The plugin exited with status 0. Failed/crashing invocations are not tracked, matching Cobra's `PersistentPostRun` semantics for built-in commands.

## Adding an Entire-Shipped Plugin to the Allowlist

When publishing an Entire-owned plugin (e.g. `entire-pgr`):

1. Append the plugin name to `officialPlugins` in `cmd/entire/cli/plugin_official.go`.
2. Match must be exact and case-sensitive — the binary on disk is `entire-<name>`.
3. Update or add tests if the plugin has unusual telemetry shape.

Once allowlisted, `cli_plugin_executed` events for that plugin will flow through the existing PostHog pipeline.

## Comparison with the Agent Protocol

| | Plugin Dispatch | [Agent Protocol](external-agent-protocol.md) |
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

The dispatcher lives in `cmd/entire/cli/plugin.go`. The entry point is `MaybeDispatchPlugin(ctx, rootCmd, args)`, called from `cmd/entire/main.go` before `rootCmd.ExecuteContext`. Returns `(handled bool, exitCode int)` — when `handled` is true, the caller exits with `exitCode`; otherwise it falls through to normal Cobra execution.

Key files:

- `cmd/entire/cli/plugin.go` — dispatcher, `resolvePlugin`, `runPlugin`
- `cmd/entire/cli/plugin_official.go` — `officialPlugins` allowlist, `IsOfficialPlugin`
- `cmd/entire/cli/telemetry/detached.go` — `BuildPluginEventPayload`, `TrackPluginDetached`
- `cmd/entire/cli/integration_test/plugin_dispatch_test.go` — end-to-end coverage of the early-dispatch path
