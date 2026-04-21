# Checkpoints v2 Manual Test Plan (Command-First)

## Purpose

This is the command-first manual QA plan for the current checkpoints v2 implementation.

Goals:
- Validate current v2 split-ref write and read behavior.
- Validate local-missing/remote-present fetch behavior.
- Validate cleanup, doctor, attach, and migration flows that now exist in the CLI.
- Provide copy-paste test steps and evidence collection.

## v2 Invariants

- Permanent metadata + compact transcripts live at `refs/entire/checkpoints/v2/main`
- Active raw resumable transcripts live at `refs/entire/checkpoints/v2/full/current`
- Archived raw transcript generations live at `refs/entire/checkpoints/v2/full/<generation>`
- v1 compatibility still exists for fallback and migration scenarios unless running `checkpoints_version: 2`
- v2 path layout differs by ref:
  - `/main` contains `metadata.json`, session `metadata.json`, `prompt.txt`, `transcript.jsonl`, `transcript_hash.txt`
  - `/full/current` contains `raw_transcript` and `raw_transcript_hash.txt`

## Current v2 command support status

- `entire resume`: supports v2 reads and v1 fallback.
- `entire explain`: supports v2 reads, compact transcript reads from `/main`, fetch-on-miss, and v1 fallback.
- `entire attach`: writes v2 when checkpoints v2 is enabled, while still supporting v1 when not.
- `entire migrate --checkpoints v2`: implemented and hidden; migrates v1 committed checkpoints into v2.
- `entire doctor`: includes v2-specific health checks in addition to the legacy metadata-branch/session checks.
- `entire clean`: includes v2 archived-generation cleanup in addition to session/shadow cleanup.
- `entire rewind`: included mainly as a regression guard; it is not a primary v2 storage validation command.

## Global Test Setup

### Test topology

Use three clones of one test repository:
- `repo-a`: primary writer and command runner
- `repo-b`: secondary writer for concurrency and remote edge cases
- `repo-fresh`: clean clone for fetch-on-demand tests

Optional: a dedicated checkpoint remote repository for `checkpoint_remote` scenarios.

### Settings

Set strategy options in `.entire/settings.json`:

```json
{
  "strategy_options": {
    "checkpoints_v2": true,
    "push_v2_refs": true,
    "full_transcript_generation_retention_days": 14
  }
}
```

Notes:
- `checkpoints_v2: true` enables dual-write/read behavior.
- `push_v2_refs: true` pushes `refs/entire/checkpoints/v2/main` and `refs/entire/checkpoints/v2/full/current`.
- `full_transcript_generation_retention_days` controls cleanup of archived `/full/<generation>` refs.
- `checkpoints_version: 2` is also supported and is the stricter v2-only mode:

```json
{
  "strategy_options": {
    "checkpoints_version": 2,
    "full_transcript_generation_retention_days": 14
  }
}
```

In `checkpoints_version: 2` mode:
- v2 is always enabled
- v2 refs are always pushed
- new writes should skip v1

### Baseline checks (run before each command section)

```bash
# Verify v2 metadata ref exists locally
git show-ref --verify -- refs/entire/checkpoints/v2/main
# Verify v2 raw current-generation ref exists locally
git show-ref --verify -- refs/entire/checkpoints/v2/full/current
# List all local v2 raw refs (current + archived generations)
git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/full/*'
# Verify legacy v1 branch exists for fallback tests
git show-ref --verify -- refs/heads/entire/checkpoints/v1
```

## Shared Inspection Toolkit

### Ref checks

Use this block to inspect local and remote v2 ref state.

```bash
# Local v2 metadata ref hash
git show-ref -- refs/entire/checkpoints/v2/main
# Local v2 raw current ref hash
git show-ref -- refs/entire/checkpoints/v2/full/current
# All local v2 raw refs with object IDs
git for-each-ref --format='%(refname:short) %(objectname)' 'refs/entire/checkpoints/v2/full/*'
# Remote view of all v2 refs on origin
git ls-remote origin 'refs/entire/checkpoints/v2/*'
```

### Checkpoint shard helper

Use this helper to derive `<shard_path>` from a checkpoint ID.

Use the reusable executable script to determine the shard path.

```bash
# Ensure helper directory exists
mkdir -p scripts

cat > scripts/checkpoint-shard-path <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

checkpoint_id="${1:-}"
if [ -z "$checkpoint_id" ]; then
  echo "usage: checkpoint-shard-path <checkpoint-id>" >&2
  exit 1
fi

echo "${checkpoint_id:0:2}/${checkpoint_id:2}"
EOF

chmod +x scripts/checkpoint-shard-path

checkpoint_id="a3b2c4d5e6f7"
shard_path="$(scripts/checkpoint-shard-path "$checkpoint_id")"
echo "$shard_path"
```

### Tree/file checks

Use this block to inspect checkpoint files on v2 refs.

```bash
# List checkpoint subtree in v2 permanent ref
git ls-tree --name-only refs/entire/checkpoints/v2/main "$shard_path"
# List checkpoint subtree in v2 raw current ref
git ls-tree --name-only refs/entire/checkpoints/v2/full/current "$shard_path"
# Read checkpoint summary metadata
git show "refs/entire/checkpoints/v2/main:${shard_path}/metadata.json"
# Read compact transcript from /main
git show "refs/entire/checkpoints/v2/main:${shard_path}/0/transcript.jsonl"
# Read compact transcript hash from /main
git show "refs/entire/checkpoints/v2/main:${shard_path}/0/transcript_hash.txt"
# Read raw transcript from /full/current
git show "refs/entire/checkpoints/v2/full/current:${shard_path}/0/raw_transcript"
# Read raw transcript hash from /full/current
git show "refs/entire/checkpoints/v2/full/current:${shard_path}/0/raw_transcript_hash.txt"
```

### Archived generation checks

Use this block to inspect archived v2 raw generations.

```bash
# List archived v2 raw generation refs
git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/full/[0-9]*'
# Read generation metadata for retention validation
git show refs/entire/checkpoints/v2/full/0000000000001:generation.json
# Check if a checkpoint exists in a specific archived generation
git ls-tree --name-only refs/entire/checkpoints/v2/full/0000000000001 "$shard_path"
```

### v1 fallback checks

Use this block to inspect legacy v1 fallback data.

```bash
# Verify v1 checkpoint branch exists
git show-ref --verify -- refs/heads/entire/checkpoints/v1
# Check checkpoint shard path on v1
git ls-tree --name-only entire/checkpoints/v1 "$shard_path"
# Read raw transcript from v1 fallback
git show "entire/checkpoints/v1:${shard_path}/0/full.jsonl"
```

## Custom Ref Primer (for this guide)

Use this section when a scenario asks you to add/remove or inspect v2 refs directly.

```bash
# List local v2 refs
git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/*'

# Delete one local ref (safe in disposable clone)
git update-ref -d refs/entire/checkpoints/v2/main

# Delete another local ref
git update-ref -d refs/entire/checkpoints/v2/full/current

# Delete all local archived generation refs
for ref in $(git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/full/[0-9]*'); do
  git update-ref -d "$ref"
done

# Verify what still exists locally
git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/*'

# Verify what exists on origin (remote refs are unchanged by local delete)
git ls-remote origin 'refs/entire/checkpoints/v2/*'
```

Notes:
- `git update-ref -d <ref>` deletes a **local ref pointer** only; it does not delete objects from remote.
- Destructive ref setup means intentionally deleting local checkpoint refs to simulate missing-data scenarios.
- Do this only in `repo-fresh` (or another disposable clone) so you do not lose local checkpoint state in your primary working clone.
- If you accidentally delete refs in the wrong clone, recover by fetching them again:

```bash
git fetch origin refs/entire/checkpoints/v2/main:refs/entire/checkpoints/v2/main
git fetch origin refs/entire/checkpoints/v2/full/current:refs/entire/checkpoints/v2/full/current
```

## Command Test Plan

This guide is intentionally command-first and focused on common, high-signal manual scenarios. Rare edge cases and corruption cases belong in automated tests.

---

## 1) `entire resume`

- What it does: restores the session transcript and prints the resume command.
- Use it for: continuing work from a checkpointed branch/session.

### Scenario 1: Baseline v2 resume (Core)

Setup:
1. In `repo-a`, enable `checkpoints_v2=true` and `push_v2_refs=true`.
2. Create a feature branch and produce at least one checkpoint.
3. Switch away from the feature branch.

Run:
1. Execute `entire resume <feature-branch>`.

Checks:

```bash
# Local v2 metadata ref hash
git show-ref -- refs/entire/checkpoints/v2/main
# Local v2 raw current ref hash
git show-ref -- refs/entire/checkpoints/v2/full/current
# All local v2 raw refs with object IDs
git for-each-ref --format='%(refname:short) %(objectname)' 'refs/entire/checkpoints/v2/full/*'
# Remote view of all v2 refs on origin
git ls-remote origin 'refs/entire/checkpoints/v2/*'
```

Expected:
- Session restored and resume command printed.
- Checkpoint data resolves from v2 (`/full/current`).

### Scenario 2: Local missing, remote present (Core)

Setup:
1. Use `repo-fresh` clone (preferred), or delete local v2 refs in disposable clone.
2. Confirm refs exist on remote.

```bash
git update-ref -d refs/entire/checkpoints/v2/main
git update-ref -d refs/entire/checkpoints/v2/full/current
for ref in $(git for-each-ref --format='%(refname)' 'refs/entire/checkpoints/v2/full/[0-9]*'); do
  git update-ref -d "$ref"
done
git ls-remote origin 'refs/entire/checkpoints/v2/*'
```

Run:
1. Execute `entire resume <feature-branch>`.

Expected:
- Resume fetches required data and succeeds when remote data exists.

### Scenario 3: v1 fallback (Optional)

Setup:
1. Create checkpoint with `checkpoints_v2=false`.
2. Enable `checkpoints_v2=true` afterward.

Run:
1. Execute `entire resume <branch-with-v1-only-checkpoint>`.

Expected:
- Resume succeeds via v1 fallback path.

### Pass checklist

- [ ] Baseline v2 resume validated.
- [ ] Missing-local fetch validated.
- [ ] v1 fallback validated.

---

## 2) `entire explain`

- What it does: reads checkpoint transcript data and explains context.
- Use it for: understanding what changed and why at a checkpoint.

Note: compact transcript behavior is supported for `entire explain`; broader v2 behavior continues to evolve.

### Scenario 1: Compact transcript path (Core)

Setup:
1. Create or identify a checkpoint where compact transcript exists on v2 `/main`.
2. Verify setup before running explain:

```bash
git show "refs/entire/checkpoints/v2/main:${shard_path}/0/transcript.jsonl"
```

Run:
1. Execute `entire explain <checkpoint-id-or-target>`.

Expected:
- Explain uses compact transcript from v2 `/main`.

### Scenario 2: v1 fallback (current compatibility, Optional)

Setup:
1. Use branch/checkpoint where data exists only in v1.

Run:
1. Execute `entire explain <target>`.

Expected:
- Explain succeeds via v1 fallback.

### Pass checklist

- [ ] Compact transcript path validated.
- [ ] v1 fallback validated.

---

## 3) `entire doctor`

- What it does: diagnoses and fixes disconnected `entire/checkpoints/v1` metadata branches and stuck sessions.
- Use it for: recovering from session-state/shadow-branch issues and metadata-branch divergence.

### Scenario 1: No issues detected (Core)

Setup:
1. Ensure no stale sessions are stuck in `ACTIVE` for >1h.
2. Ensure no `ENDED` sessions have uncondensed checkpoint data.
3. Ensure local and remote `entire/checkpoints/v1` are connected.

Run:
1. Execute `entire doctor`.

Checks:

```bash
# List current session states
ls .git/entire-sessions
# List shadow branches
git branch --list 'entire/*'
# Inspect local metadata branch tip
git log --oneline -1 entire/checkpoints/v1
# Inspect remote metadata branch tip
git ls-remote origin refs/heads/entire/checkpoints/v1
```

Expected:
- Doctor reports no disconnected metadata issue and no stuck sessions.

### Scenario 2: Stuck session handling (Optional)

Setup:
1. Create a session in `ACTIVE` and make it stale (or create an `ENDED` session with uncondensed data).
2. Confirm session state file and shadow branch exist.

```bash
# Session states present
ls .git/entire-sessions
# Shadow branches present
git branch --list 'entire/*'
```

Run:
1. Execute `entire doctor`.
2. Choose `Condense`, `Discard`, or `Skip` at the prompt (or use `--force` for auto-fix).

Expected:
- Doctor detects stuck sessions and applies selected remediation.
- After condense/discard, corresponding session state/shadow-branch data is reduced or removed.

### Pass checklist

- [ ] No-issue case reports clean status.
- [ ] Stuck-session detection and remediation validated.

---

## 4) `entire clean`

- What it does: cleans session state and shadow branch data for current HEAD, or orphaned Entire data with `--all`.
- Use it for: removing stale session metadata and orphaned Entire artifacts.

### Scenario 1: Current HEAD cleanup (Core)

Setup:
1. In `repo-a`, create a session on the current HEAD so there is session state and shadow-branch data.

Run:
1. Execute `entire clean --dry-run` and review the preview.
2. Execute `entire clean` and confirm the prompt.

Checks:

```bash
# List shadow branches before/after
git branch --list 'entire/*'
# List local session state files before/after
ls .git/entire-sessions
```

Expected:
- `--dry-run` lists items without deleting.
- `entire clean` removes current-HEAD session state and shadow branch data.

### Scenario 2: Active-session guard and `--force` (Optional)

Setup:
1. Start an active session on current HEAD.

Run:
1. Execute `entire clean` without `--force`.
2. Execute `entire clean --force`.

Expected:
- Without `--force`, command warns and refuses to clean active session data.
- With `--force`, command proceeds.

### Scenario 3: Repository-wide orphan cleanup (`--all`, Optional)

Setup:
1. Create orphaned data (for example: old shadow branches or orphaned session-state files).

Run:
1. Execute `entire clean --all --dry-run`.
2. Execute `entire clean --all`.

Expected:
- Dry-run previews orphaned items.
- `--all` removes orphaned items and temporary files.
- `entire/checkpoints/v1` branch is preserved.

### Pass checklist

- [ ] Current-HEAD cleanup behavior validated.
- [ ] Active-session guard and `--force` behavior validated.
- [ ] `--all` orphan cleanup behavior validated.

---

## 5) `entire migrate`

- What it does: migrates v1 checkpoint storage into v2 split refs.
- Use it for: upgrading repositories with existing v1 checkpoint history.

Note: `entire migrate` is not implemented yet. Treat this section as a forward-looking validation plan and run it only after the command becomes available.

### Scenario 1: First migration run (Core, when command is available)

Setup:
1. Prepare repository with v1-only checkpoint history.

Run:
1. Execute `entire migrate`.

Checks:
Run this block before migration:

```bash
# Verify v1 checkpoint branch exists
git show-ref --verify -- refs/heads/entire/checkpoints/v1
# Check checkpoint shard path on v1
git ls-tree --name-only entire/checkpoints/v1 "$shard_path"
# Read raw transcript from v1 fallback
git show "entire/checkpoints/v1:${shard_path}/0/full.jsonl"
```

Run this block after migration:

```bash
# Local v2 metadata ref hash
git show-ref -- refs/entire/checkpoints/v2/main
# Local v2 raw current ref hash
git show-ref -- refs/entire/checkpoints/v2/full/current
# List checkpoint subtree in v2 permanent ref
git ls-tree --name-only refs/entire/checkpoints/v2/main "$shard_path"
# List checkpoint subtree in v2 raw current ref
git ls-tree --name-only refs/entire/checkpoints/v2/full/current "$shard_path"
# Read checkpoint summary metadata
git show "refs/entire/checkpoints/v2/main:${shard_path}/metadata.json"
# Read compact transcript (when available)
git show "refs/entire/checkpoints/v2/main:${shard_path}/0/transcript.jsonl"
# Read raw transcript
git show "refs/entire/checkpoints/v2/full/current:${shard_path}/0/full.jsonl"
```

Expected:
- v1 checkpoints are migrated to v2 split refs.

### Pass checklist

- [ ] Migration output correctness validated.
- [ ] Idempotency and non-compaction checks moved to automated tests.

---

## 6) `entire attach`

- What it does: attaches an existing agent transcript/session to checkpoint metadata when hooks did not capture it.
- Use it for: recovering missed checkpoints or linking research sessions to the latest commit.

Note: full checkpoints v2 behavior for `entire attach` is still in progress and will be updated soon.

### Scenario 1: Attach creates a new checkpoint and trailer (Core)

Setup:
1. Ensure repository has at least one commit.
2. Ensure target session transcript exists on disk for the selected agent.
3. Ensure latest commit does not already contain `Entire-Checkpoint` trailer.

Run:
1. Execute `entire attach <session-id>`.
2. When prompted, allow trailer amendment (or rerun with `--force`).

Checks:

```bash
# Verify latest commit includes Entire-Checkpoint trailer
git log -1 --pretty=%B
# Verify checkpoint metadata exists on v1 branch
git show-ref --verify -- refs/heads/entire/checkpoints/v1
```

Expected:
- Attach reports a created checkpoint ID.
- Latest commit includes `Entire-Checkpoint: <id>` trailer (when amendment accepted/forced).
- Session metadata is written to `entire/checkpoints/v1`.

### Scenario 2: Attach adds session to existing checkpoint (Optional)

Setup:
1. Ensure latest commit already has `Entire-Checkpoint` trailer (this reuses that checkpoint; it does not block attaching additional sessions).
2. Ensure a second transcript/session exists to attach.

Run:
1. Execute `entire attach <session-id>`.

Checks:

```bash
# Capture checkpoint ID from latest commit
git log -1 --pretty=%B
# Inspect checkpoint files on v1 branch for multiple session folders
checkpoint_id="<id-from-commit-trailer>"
shard_path="$(scripts/checkpoint-shard-path "$checkpoint_id")"
git ls-tree --name-only entire/checkpoints/v1 "$shard_path"
```

Expected:
- Attach reports that it added to existing checkpoint.
- Checkpoint now contains additional session data.

### Scenario 3: Attached session appears correctly in v2 refs (future behavior, Optional)

Setup:
1. Enable v2 mode in test settings (`checkpoints_v2=true`).
2. Start from a commit with an `Entire-Checkpoint` trailer.
3. Attach a new session to that checkpoint.

Run:
1. Execute `entire attach <session-id>`.

Checks:

```bash
# Read checkpoint ID from latest commit trailer
git log -1 --pretty=%B
# Set checkpoint id manually from trailer output
checkpoint_id="<id-from-commit-trailer>"
shard_path="$(scripts/checkpoint-shard-path "$checkpoint_id")"

# Validate checkpoint metadata/session content on v2 main
git ls-tree --name-only refs/entire/checkpoints/v2/main "$shard_path"
git show "refs/entire/checkpoints/v2/main:${shard_path}/metadata.json"

# Validate resumable transcript artifacts on v2 full/current
git ls-tree --name-only refs/entire/checkpoints/v2/full/current "$shard_path"
```

Expected:
- Attached session is represented in v2 checkpoint metadata.
- Required transcript artifacts are present in v2 refs for follow-up commands (`resume`/`explain`).

### Pass checklist

- [ ] New-checkpoint attach flow validated.
- [ ] Existing-checkpoint attach flow validated.
- [ ] v2 ref representation for attached sessions validated.

---

## 7) `entire rewind` (regression guard)

- What it does: restores repository files/logs to a prior checkpoint.
- Use it for: undoing recent changes and returning to earlier state.

### Scenario 1: Rewind normal flow (Core)

Setup:
1. Create at least two temporary checkpoints in one session.
2. Modify files between checkpoints.

Run:
1. Execute `entire rewind` and select an earlier checkpoint.

Expected:
- Files and prompt/log context restore to selected checkpoint state.

### Pass checklist

- [ ] Rewind still restores expected files and prompt context.

## Evidence Collection (for every run)

Capture:
- executed command + full output
- before/after `git show-ref` snapshots
- `git ls-remote` snapshots when remote behavior is involved
- `git show` and `git ls-tree` evidence for expected files/paths
- outcome classification: pass/fail/blocked with reason

## Exit Criteria

### Current rollout gate

Current manual validation is complete when:
- `resume`, `doctor`, `clean`, and `rewind` pass applicable scenarios
- `explain` compact transcript scenario passes
- remote fetch paths pass in missing-local situations

### Future rollout gate

Future manual validation is complete when:
- `attach` v2 representation scenario passes with fully shipped v2 behavior
- `migrate` scenario passes after the command is implemented
- expanded edge-case/corruption checks remain covered by automated tests
