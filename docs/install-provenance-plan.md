# Install Provenance And Auto-Update Plan

## Goal

Add installer-owned install provenance for released binaries, then build auto-update on top of that provenance.

## Scope For The Next PR

1. Define the provenance file format.
   Example fields:
   - `manager`: `install.sh`, `brew`, `scoop`
   - `channel`: `stable`, `nightly`
   - `package`: `entire`, `entire@nightly`, `entire/cli`
   - `installed_at`: timestamp

2. Make each installer write the provenance file.
   - `install.sh` writes it directly.
   - Homebrew formula/cask writes it in package-managed install hooks.
   - Scoop manifest writes it in `post_install`.

3. Keep the CLI read-only for provenance.
   - The CLI reads `install.json`.
   - If the file is missing, it may infer update hints for backward compatibility.
   - The CLI must not create or overwrite provenance.

4. Add auto-update on top of installer-owned provenance.
   - Only offer auto-update when the manager/channel combination supports it.
   - Map auto-update actions to installer-native commands.
   - Reuse the same provenance source for update prompts and execution.

## Migration Strategy

- Existing installs without provenance continue using heuristic detection.
- New installs write provenance at install time.
- After installer coverage is in place, auto-update can require provenance for managed flows.

## Open Questions

- Exact location of `install.json` on each platform.
- Whether Homebrew nightly should be recorded as `manager=brew` + `channel=nightly` or with a distinct package token only.
- Whether Go-installed/dev builds should remain unsupported for auto-update.
