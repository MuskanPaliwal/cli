# Claude Code Stop transcript readiness

Status: Proposed and manually validated

Scope: Entire CLI issue [#2091](https://github.com/entireio/cli/issues/2091), item 3 only

Related change: [PR #2002](https://github.com/entireio/cli/pull/2002)

## Decision summary

Replace Claude Code's Stop-only prepare-then-read sequence with one capture
operation that returns the exact bytes it validated.

Claude Code versions that provide `last_assistant_message` use a fast semantic
readiness check. Versions that omit the field retain the existing 500 ms quiet
window. Both paths validate a single owned snapshot, honor cancellation, and
return no bytes on timeout or uncertainty.

The snapshot must drive every Stop consumer. Checkpoint finalization and
transcript-position updates must not reopen Claude's mutable transcript path.

Do not replace `TranscriptPreparer` globally in this change. OpenCode uses that
interface to materialize transcripts, and changing it would broaden this work
beyond issue #2091 item 3.

## Problem

Claude Code writes its JSONL transcript asynchronously. Entire's current
`waitForTranscriptFlush` logic has two readiness paths:

1. Search for `hooks claude-code stop` in a `hook_progress` record.
2. If the sentinel is absent, require the file size to stay unchanged for
   500 ms.

The sentinel is dead in practice. It appeared zero times in 40 transcripts
reported in the issue, zero times in 206 local transcripts inspected during
this investigation, and zero times in the three real Stop traces described
below.

Claude Code writes `stop_hook_summary` only after the Stop hook returns. A hook
cannot wait for a record whose producer is waiting for that hook to finish.

The 500 ms fallback avoids mistaking a brief pause for completion, but it adds
about 520 ms to a healthy Stop hook.

## The ownership bug

Improving the wait loop alone does not make the read safe. The current
`TranscriptPreparer` interface validates a path but returns no bytes, handle,
identity, size, or version.

The current Stop flow can observe three different file states:

```text
T0  PrepareTranscript validates state A.
T1  Claude appends to, truncates, or replaces the file.
T2  lifecycle ReadTranscript reads state B.
T3  lifecycle copies and parses state B.
T4  checkpoint finalization reopens the live path and reads state C.
```

This is a time-of-check/time-of-use race. Even a perfect readiness predicate at
T0 cannot make states B and C equal to A.

The correct ownership transition is:

```text
Claude-owned mutable transcript
             |
             | CaptureTranscript
             v
Entire-owned snapshot
             |
             +-- session copy
             +-- modified-file extraction
             +-- token calculation
             +-- transcript-position update
             +-- checkpoint finalization
```

## Readiness means turn content, not permanent EOF

Real traces showed that Claude can append metadata after the Stop hook returns.
No Stop hook can observe a permanently final file if the producer deliberately
writes another record after that hook.

The useful readiness condition is therefore:

> The complete conversational content required to capture this turn is present
> in a syntactically complete snapshot.

The condition is not:

> The transcript path will never change again.

In current Claude Code traces, the final assistant response was already present
during Stop. Claude appended a `last-prompt` metadata record shortly after Stop
returned. Entire has no parser or lifecycle behavior that depends on
`last-prompt`; prompts are captured separately.

## Manual producer traces

The timing probe ran in an isolated temporary project. User and project
settings were excluded. The hook recorded only timestamps, file size, record
count, JSONL completeness, and whether the hook-provided final response matched
an assistant record. It did not log response or transcript content.

### Current Claude Code 2.1.238, one-line response

```text
0.0 ms    Stop hook started
52.5 ms   9 records, complete JSONL, final response present
377.3 ms  Stop hook returning
391.4 ms  10 records, complete JSONL, final response still present
```

The record appended after return was `last-prompt` metadata.

### Current Claude Code 2.1.238, two-line response

```text
0.0 ms     Stop hook started
69.7 ms    9 records, complete JSONL, final response present
1086.7 ms  Stop hook returning
1097.3 ms  10 records, complete JSONL, final response still present
```

Holding the Stop hook for one second did not cause more transcript writes. The
`last-prompt` record appeared about 10 ms after return. The two-line response
matched the text reconstructed from the transcript.

### Older Claude Code 2.1.45

```text
0.0 ms     Stop hook started
52.3 ms    3 records, complete JSONL, last record is assistant
1072.2 ms  Stop hook returning
2071.8 ms  monitor finished with no observed growth
```

The Stop payload did not contain `last_assistant_message`, as expected. The
transcript was already complete and stable in this trace.

Claude Code added `last_assistant_message` in version 2.1.47. Entire currently
does not enforce a minimum Claude Code version, so the missing-field path is a
supported compatibility case.

The three successful real runs cost $0.0071632. An earlier harness run cost
$0.01414 but produced no timing evidence because its background observer used
a Ruby method unavailable in macOS Ruby 2.6. Total spend was $0.0213032.

## Proposed interface

Normalize the optional producer evidence on the lifecycle event:

```go
type Event struct {
    // Existing fields omitted.
    FinalResponse        *string
    FinalResponsePresent bool
}
```

`FinalResponsePresent` distinguishes an omitted field from explicit `null`.
`FinalResponse` distinguishes `null` from empty and non-empty strings. Missing,
null, and empty values are unusable as readiness markers and follow the legacy
path.

The turn-start state also records whether its transcript position was measured
successfully. A measured zero is a valid first-turn boundary; zero left by an
analyzer error is not. Modern capture fails closed without this validity bit.
Positive positions from older state files are unambiguous and can be migrated
as measured, while an older zero remains ambiguous.

Add a small optional capture interface at the agent seam:

```go
type TranscriptCaptureRequest struct {
    SessionRef    string
    StartPosition int
    FinalResponse *string
}

type TranscriptSnapshot struct {
    Data     []byte
    Position int
}

type TranscriptCapturer interface {
    Agent

    CaptureTranscript(
        ctx context.Context,
        request TranscriptCaptureRequest,
    ) (TranscriptSnapshot, error)
}
```

A successful result means:

- `Data` contains the exact bytes whose readiness was validated.
- The caller owns the returned slice and treats it as read-only.
- `Position` was calculated from those bytes, not from a later path read.
- The source file changing after return cannot change `Data` or `Position`.
- Any usable producer evidence was satisfied.
- The current turn ends in a syntactically complete JSONL record.

The lifecycle layer should use the optional interface for Claude Stop. Other
agents keep their current prepare-and-read behavior in this scoped change.

## Modern readiness path

Use this path when `FinalResponse` is non-nil and non-empty.

1. Observe the transcript fingerprint.
2. Open the file and record the handle's identity and size.
3. Read exactly the observed byte range from that handle.
4. Re-stat the handle and path.
5. Retry if the file grew, shrank, changed identity, was replaced, or was
   rewritten during the read.
6. Require the final nonblank JSONL record to parse.
7. Parse records after `StartPosition`.
8. Reconstruct the latest non-empty assistant text in the current turn.
9. Require it to equal `FinalResponse`.
10. Return those exact bytes and their position.

Do not use a raw substring search. JSON escaping, repeated responses, tool
results, and user text can all create false matches.

`StartPosition` prevents a repeated response from an earlier turn satisfying
the current turn:

```text
Turn 1 assistant: "Done."
Turn 2 Stop payload: last_assistant_message = "Done."
Turn 2 response has not reached the transcript yet.
```

Searching the whole session would find Turn 1 and return early. Searching after
the current turn's starting position does not.

A supplied modern marker that never matches must time out with an error. It
must not silently downgrade to the legacy heuristic because that would hide a
producer-contract violation.

The modern path has no mandatory polling delay. Expected healthy latency is one
transcript read and validation; polling is used only to retry a rejected or
changing candidate.

## Legacy readiness path

Use this path when `FinalResponse` is nil or empty.

1. Remove all sentinel checks.
2. Require the file fingerprint to remain unchanged for the existing 500 ms
   quiet window.
3. Reset the window on growth, truncation, rewrite, or replacement.
4. Capture and validate an exact snapshot using the same open-read-restat
   procedure as the modern path.
5. Require a syntactically complete final nonblank JSONL record.
6. Return those exact bytes and their position.

Do not shorten the legacy path to two polls. Two equal observations prove only
that no write happened during that interval. They do not prove that an older
producer will not append later.

The legacy path remains a timing heuristic. No file-only observer can prove
future silence. Absolute semantic certainty for versions without producer
evidence would require deferred capture or an explicit minimum supported Claude
Code version. This design preserves existing compatibility and does not weaken
the current quiet-window protection.

## Snapshot acquisition details

Use a private fingerprint containing at least:

- file identity;
- size;
- modification time or the strongest portable modification token available.

Identity, size, and modification time detect ordinary rewrites that advance the
filesystem timestamp. They cannot portably prove that a same-size rewrite did
not occur when the filesystem reports the same timestamp. That limitation is
acceptable for Claude's append-oriented transcript producer; tests must not
claim stronger rewrite detection than the fingerprint provides.

Poll fingerprints, not full file contents. The modern path reads its initial
candidate immediately because producer evidence determines readiness. The
legacy path reads only after the fingerprint satisfies the quiet window.

After opening the candidate file:

1. Stat the open handle.
2. Read exactly the observed size.
3. Stat the handle again.
4. Stat the path again.
5. Confirm the path still names the same file and the handle did not grow,
   shrink, or change during the read.

Holding an open handle protects against path replacement but not truncation or
append of the same file. The before-and-after checks are still required.

Use a ticker or timer with `select` on `ctx.Done()`. Do not use unconditional
`time.Sleep`.

Keep timing policy private to the Claude implementation:

```go
type captureConfig struct {
    pollInterval time.Duration
    quietWindow  time.Duration
    maxWait      time.Duration
}
```

Production defaults can remain 50 ms, 500 ms, and 3 seconds. Tests may use
shorter internal values and explicit writer coordination. These values should
not become part of the public interface.

## Consumer changes

On successful capture, the lifecycle handler should use `snapshot.Data` for:

- the Entire-owned session copy;
- Claude main-transcript modified-file extraction;
- token calculation;
- any current-turn transcript parsing.

Pass the same snapshot into turn-end finalization. The strategy should use it
instead of calling `os.ReadFile` on `SessionState.TranscriptPath`.

Use `snapshot.Position` when advancing the turn-end transcript boundary.
Do not call path-based `GetTranscriptPosition` for the same Stop transaction.

Keep the original source path only for operations whose meaning is genuinely
path-based, such as locating Claude's separate subagent transcript directory or
persisting the agent session reference.

Do not hide raw snapshot bytes in `context.Context` or persisted session state.
Pass them explicitly through the in-process Stop call chain.

## Error behavior

The capture module should expose one main readiness classification:

```go
var ErrTranscriptNotReady = errors.New("transcript not ready")
```

Missing, stale, incomplete, replaced, continuously growing, and timed-out
transcripts may wrap this error with diagnostic detail. The caller has the same
data decision in every case: no snapshot means no transcript consumption,
condensation, or checkpoint finalization.

Cancellation must preserve `context.Canceled` or `context.DeadlineExceeded` so
callers and tests can distinguish it with `errors.Is`.

The hook-level session-state recovery behavior should remain separate from the
capture interface. Implementation must verify that a failed capture does not
publish uncertain data and does not leave session state in an unrecoverable
phase. Preserve provisional mid-turn checkpoints rather than replacing them
with uncertain transcript bytes.

## File cases

| Case | Modern path | Legacy path |
| --- | --- | --- |
| Stable complete transcript | Return after marker match and stable polls | Return after 500 ms quiet window |
| Partial final JSON record | Keep waiting | Keep waiting |
| Continued growth | Reset observations | Reset quiet window |
| Pause shorter than 500 ms, then append | Marker must still match final assistant text | Do not return during pause |
| Truncation | Restart against new fingerprint | Restart quiet window |
| Path replacement | Restart against new identity | Restart quiet window |
| Missing file | Return no snapshot | Return no snapshot |
| Stale file | Return no snapshot | Return no snapshot |
| Modern marker missing from snapshot | Keep waiting, then error | Not applicable |
| Timeout | Return no snapshot | Return no snapshot |
| Context cancellation | Return no snapshot and preserve context error | Same |
| Source changes after success | Returned snapshot stays unchanged | Same |

## Tests

Tests should cross the capture interface and use real files under `t.TempDir()`.
They should assert returned bytes and errors, not narrow timing margins.

Required focused coverage:

1. Stable complete modern transcript returns an owned snapshot.
2. The modern response must match after the turn-start position.
3. A repeated response from an earlier turn does not satisfy readiness.
4. A partial final JSON record is not returned and succeeds after completion.
5. Continued growth resets readiness.
6. A pause followed by more writes does not return an early legacy snapshot.
7. Truncation restarts readiness.
8. Same-path replacement restarts readiness.
9. Missing and stale files return no snapshot.
10. Timeout returns no snapshot.
11. Cancellation returns no snapshot and preserves the context error.
12. Mutating the source after success does not mutate the returned bytes.
13. Turn-end checkpoint finalization uses the captured snapshot even if the
    live Claude transcript later differs.
14. The transcript-position update comes from the same snapshot.
15. Stop payload parsing distinguishes missing, null, empty, and non-empty
    `last_assistant_message` values.
16. A measured zero turn-start position is accepted, while an unmeasured zero
    is rejected before capture.

Every top-level test and subtest should call `t.Parallel()` unless it modifies
process-global state.

Avoid `time.Sleep` assertions with small margins. Coordinate writer actions with
channels, and use private shorter timing configuration only to bound test time.

## Manual verification after implementation

Repeat the real Claude probes used for this design:

1. Current Claude Code with a one-line response.
2. Current Claude Code with a multi-line response and a one-second Stop hook.
3. A pre-2.1.47 Claude Code release through an isolated package cache.

Run controlled manual writer scenarios for partial JSON, continued growth, a
pause followed by append, truncation, replacement, timeout, cancellation, and
source mutation after capture.

Report production trace evidence separately from deterministic test timing.

## Validation commands

Run focused Claude Code agent tests first. Then run the nearest lifecycle and
strategy tests affected by snapshot threading. Finish with the repository's
required checks from `AGENTS.md`.

Do not run paid real-agent E2E tests except for an explicitly approved manual
probe. The Vogon canary is local and remains part of the repository's normal CI
checks.

Measure the healthy path before and after:

- current stable fallback baseline, expected near 520 ms;
- modern snapshot path, with no mandatory polling delay;
- legacy path, expected to remain near 520 ms.

Use broad deterministic timing bounds in tests. Treat the real Claude traces as
production evidence, not deterministic performance tests.

## Implementation sequence

1. Add failing payload and capture-interface tests.
2. Parse `last_assistant_message` into normalized value and presence fields.
3. Add the scoped transcript capture types and optional interface.
4. Implement Claude fingerprint polling, snapshot reads, JSONL validation, and
   current-turn final-response matching.
5. Route Claude Stop through capture while preserving existing preparation for
   other agents and other call sites.
6. Thread the snapshot into turn-end finalization and position updates.
7. Add lifecycle and strategy regression coverage for the snapshot guarantee.
8. Run focused tests and inspect their actual inputs and outputs.
9. Repeat manual producer and controlled-writer probes.
10. Run repository checks and confirm the final diff contains only issue #2091
    item 3 work plus this design documentation.

## Non-goals

Do not include:

- condensation batching;
- session pruning;
- the unexplained three-second latency floor;
- full-redaction reuse;
- subagent transcript atomicity;
- a repository-wide replacement of `TranscriptPreparer`;
- an external-agent capability protocol change;
- a minimum Claude Code version policy.

## Alternatives rejected

### Patch only `waitForTranscriptFlush`

This leaves prepare-then-read and finalization re-read races intact.

### Change `TranscriptPreparer` to return bytes everywhere

This is a good eventual direction but affects OpenCode materialization, attach,
condensation, and other mid-turn callers. It is wider than item 3.

### General evidence and snapshot object framework

A general framework with evidence interfaces, source references, read
references, temporary snapshot paths, and cleanup could support future agents.
Only Claude needs producer evidence today. The extra interface would be mostly
hypothetical.

### Two stable polls for old Claude Code

This reduces a 500 ms pause tolerance to about 50 ms without adding a producer
completion signal. One successful old-version trace does not justify weakening
the fallback.

### Temporary snapshot path

Returning bytes is simpler. A temporary path adds cleanup, lifetime, permission,
and duplicate-hook concerns. Existing Claude Stop consumers already accept raw
bytes for the important operations.

## Evidence provenance

The original readiness commits have no locally available Entire checkpoint
transcripts. Their intent was reconstructed from commit messages and current
code. The ownership analysis is inferred from the present call chain.

The real Claude timing claims above come from direct manual traces. The issue,
PR, Claude hooks documentation, and Claude Code changelog should be rechecked
before implementation in case their live state changes.

## References

- [Entire CLI issue #2091](https://github.com/entireio/cli/issues/2091)
- [Entire CLI PR #2002](https://github.com/entireio/cli/pull/2002)
- [Claude Code hooks reference](https://code.claude.com/docs/en/hooks#stop-input)
- [Claude Code changelog](https://code.claude.com/docs/en/changelog#2-1-47)
