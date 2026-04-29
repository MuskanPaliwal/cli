# Security & Privacy

Entire stores AI session transcripts and metadata in your git repository. This document explains what data is stored, how sensitive content is protected, and how to configure additional safeguards.

## Transcript Storage & Git History

### Where data is stored

When you use Entire with an AI agent (Claude Code, Codex, Gemini CLI, OpenCode, Cursor, Factory AI Droid, Copilot CLI), session transcripts, user prompts, and checkpoint metadata are committed to a dedicated branch in your git repository (`entire/checkpoints/v1`). This branch is separate from your working branches, your code commits stay clean, but it lives in the same repository.

Entire also creates temporary local branches (e.g., `entire/<short-hash>`) as working storage during a session. These shadow branches store file snapshots and transcripts **without redaction**. They are cleaned up when session data is condensed (with redaction) into `entire/checkpoints/v1` at commit time. Shadow branches are **not** pushed by Entire — do not push them manually, as unredacted content would be visible on the remote.

Anyone with access to your repository can view the transcript data on the `entire/checkpoints/v1` branch. This includes the full prompt/response history and session metadata. Note that transcripts capture all tool interactions — including file contents, MCP server calls, and other data exchanged during the session.

If your repository is **public**, this data is visible to the entire internet.

### What Entire redacts automatically

Entire automatically scans transcript and metadata content before writing it to the `entire/checkpoints/v1` branch. Five secret detection methods run during condensation:

1. **Entropy scoring** — Identifies high-entropy strings (Shannon entropy > 4.5) that look like randomly generated secrets, even if they don't match a known pattern.
2. **Pattern matching** — Uses [Betterleaks](https://github.com/betterleaks/betterleaks) built-in rules to detect known secret formats.
3. **Credentialed URI detection** — Redacts URLs with embedded passwords, such as `scheme://user:password@host`.
4. **Database connection-string detection** — Redacts JDBC, Postgres keyword DSN, SQL Server, and ODBC-style connection strings containing passwords.
5. **Bounded credential value detection** — Redacts password-like config values such as `DB_PASSWORD=...` and `PGPASSWORD=...` while preserving the surrounding key.

Detected secrets are replaced with `REDACTED` before the data is ever written to a git object. This is **always on** and cannot be disabled.

### Recommendations

If your AI sessions will touch sensitive data:

- **Use a private repository.** This is the simplest and most complete protection. Transcripts on `entire/checkpoints/v1` are only visible to collaborators.
- **Avoid passing sensitive files to your agent.** Content that never enters the agent conversation never appears in transcripts.
- **Review before pushing.** You can inspect the `entire/checkpoints/v1` branch locally before pushing it to a remote.

## What Gets Redacted

### Secrets (always on)

Betterleaks pattern matching covers cloud providers (AWS, GCP, Azure), version control platforms (GitHub, GitLab, Bitbucket), payment processors (Stripe, Square), communication tools (Slack, Discord, Twilio), private key blocks (RSA, DSA, EC, PGP), and generic credentials (bearer tokens, basic auth, JWTs). Dedicated credentialed URI detection covers URLs that embed passwords. Additional database connection-string detection covers DB DSNs and query-parameter passwords not reliably covered by generic secret rules. Entropy scoring catches secrets that don't match any known pattern.

All detected secrets are replaced with `REDACTED`.

To reduce over-redaction, Entire preserves structural transcript fields such as IDs and paths, ignores common placeholder values, and redacts only credential values for bounded key/value forms. When a connection string contains a real (non-placeholder) password, it is redacted as a unit because partial fragments can still expose sensitive material; connection strings whose passwords are placeholders (e.g. `${DB_PASSWORD}`) are left intact.

## Customizing redaction

The built-in detectors handle well-known secret formats. For internal credential shapes that aren't covered (custom env-var prefixes, internal service tokens, project-specific session formats), Entire offers two extension surfaces. Both feed the same engine and run as their own layer between connection-string detection and bounded credential KV detection.

### Surface 1: Inline `redaction.custom_secrets`

Add a label → regex map under `redaction.custom_secrets` in `.entire/settings.json`:

```json
{
  "redaction": {
    "custom_secrets": {
      "acme_token":  "ACME_TOKEN_[A-Za-z0-9]{20,}",
      "internal_id": "INTERNAL_[a-z]{6}_[0-9]{4}"
    }
  }
}
```

- The label is for diagnostics only; matches are replaced with the bare `REDACTED` token (matching the built-in secret layers, not the `[REDACTED_<LABEL>]` token used for PII).
- Regexes follow [Go's RE2 syntax](https://pkg.go.dev/regexp/syntax). No lookarounds, no backreferences.
- A failed compile is logged once at startup and the rule is skipped — it will never crash the redactor.
- Override in `.entire/settings.local.json` for personal additions; entries merge per-key (override replaces the same key, leaves other keys intact).

### Surface 2: Rule packs

Drop a YAML or JSON file into `.entire/redactors/`:

```yaml
# .entire/redactors/acme-internal.yaml
name: acme-internal              # MUST match the filename stem
version: 1.0.0
description: Internal ACME service tokens
rules:
  - id: acme-token
    description: Long-lived ACME service tokens
    regex: 'ACME_TOKEN_[A-Za-z0-9]{20,}'
    samples:
      - { input: "key=ACME_TOKEN_abc123def456ghi789jkl", redacted: true  }
      - { input: "ACME_TOKEN_short",                     redacted: false }
  - id: acme-session
    regex: 'asess_[a-f0-9]{32}'
```

Equivalent JSON form:

```json
{
  "name": "acme-internal",
  "version": "1.0.0",
  "rules": [
    {
      "id": "acme-token",
      "regex": "ACME_TOKEN_[A-Za-z0-9]{20,}",
      "samples": [
        { "input": "key=ACME_TOKEN_abc123def456ghi789jkl", "redacted": true  },
        { "input": "ACME_TOKEN_short",                     "redacted": false }
      ]
    }
  ]
}
```

**Required fields:** `name` (must equal the filename stem — `acme-internal.yaml` → `acme-internal`), `version` (any string; semver recommended), and `rules[]` (at least one entry, each with `id` and `regex`).

**Optional fields:** `description` (pack-level and rule-level), and `rules[].samples[]` (see "Self-tests" below).

### Self-tests via `samples[]`

Each rule may declare an array of `{input, redacted}` pairs. On the next process startup after editing the pack, Entire runs each sample and emits a `slog.Warn` for any mismatch:

```
WARN  redactor pack sample mismatch  pack=.entire/redactors/acme-internal.yaml
      rule=acme-token sample="..." expected=true got=false
```

A failing sample never disables the rule — sample validation is informational. Use it to catch typos and false positives before they ship.

### Distribution

- **Within a team:** commit `.entire/settings.json` and/or `.entire/redactors/*` to your repo. Teammates pull and the rules apply.
- **Across teams:** copy the pack file or share a link to a gist; recipients drop the file into their `.entire/redactors/`.
- **Personal-only:** put the file in `.entire/redactors/local/` — `entire enable` writes that path into `.entire/.gitignore` so personal rules don't pollute team commits.

### When to write a rule vs. file an issue

Write a rule for internal service tokens (`ACME_*`, `INTERNAL_*`), custom env-var prefixes the bundled detectors don't know about, and project-specific session formats.

File an issue when the rule would benefit every Entire user (e.g., a major SaaS issued a new token format), when a built-in is producing false positives on common idioms in your codebase, or when a built-in is *not* catching a well-known shared format (we'd rather fix the built-in than have everyone ship the same custom rule).

### Troubleshooting

- **My rule doesn't redact anything.** Check Entire's logs (`tail -f .entire/logs/*.log`) for `slog.Warn` lines mentioning your label or pack path.
- **My pack file is silently ignored.** Filenames must end in `.yaml`, `.yml`, or `.json`. Other extensions are skipped.
- **I want to disable a rule temporarily.** Comment it out (prefix the YAML key with `#`) or remove the entry from `custom_secrets`. The rule reloads on the next CLI invocation.

## Limitations

- **Best-effort.** Novel or low-entropy secrets (short passwords, predictable tokens) may not be caught.
- **Filenames and binary data.** Secrets in filenames, binary files, or deeply nested structures may not be detected.
- **JSONL skip rules.** Entire skips scanning fields named `signature` or ending in `id`/`ids`, and objects whose `type` starts with `image` or equals `base64`, to avoid false positives.
- **Users are ultimately responsible** for reviewing what they commit and push. Redaction is a safety net, not a guarantee.
