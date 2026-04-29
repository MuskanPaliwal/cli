package redact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePack_YAMLValid(t *testing.T) {
	t.Parallel()

	body := []byte(`
name: acme-internal
version: 1.0.0
description: Internal ACME tokens
rules:
  - id: acme-token
    regex: 'ACME_TOKEN_[A-Za-z0-9]{20,}'
    samples:
      - { input: "ACME_TOKEN_abc123def456ghi789jkl", redacted: true }
      - { input: "ACME_TOKEN_short", redacted: false }
  - id: acme-session
    regex: 'asess_[a-f0-9]{32}'
`)

	pack, err := ParsePack(body, "acme-internal.yaml")
	if err != nil {
		t.Fatalf("ParsePack: %v", err)
	}
	if pack.Name != "acme-internal" {
		t.Errorf("Name: want acme-internal, have %q", pack.Name)
	}
	if pack.Version != "1.0.0" {
		t.Errorf("Version: want 1.0.0, have %q", pack.Version)
	}
	if len(pack.Rules) != 2 {
		t.Fatalf("Rules: want 2, have %d", len(pack.Rules))
	}
	if pack.Rules[0].ID != "acme-token" {
		t.Errorf("Rules[0].ID: want acme-token, have %q", pack.Rules[0].ID)
	}
	if len(pack.Rules[0].Samples) != 2 {
		t.Errorf("Rules[0].Samples: want 2, have %d", len(pack.Rules[0].Samples))
	}
	if !pack.Rules[0].Samples[0].Redacted {
		t.Error("Rules[0].Samples[0].Redacted: want true")
	}
}

func TestParsePack_JSONValid(t *testing.T) {
	t.Parallel()

	body := []byte(`{
  "name": "acme-internal",
  "version": "1.0.0",
  "rules": [
    {
      "id": "acme-token",
      "regex": "ACME_TOKEN_[A-Za-z0-9]{20,}"
    }
  ]
}`)

	pack, err := ParsePack(body, "acme-internal.json")
	if err != nil {
		t.Fatalf("ParsePack: %v", err)
	}
	if pack.Name != "acme-internal" {
		t.Errorf("Name: want acme-internal, have %q", pack.Name)
	}
	if len(pack.Rules) != 1 {
		t.Fatalf("Rules: want 1, have %d", len(pack.Rules))
	}
}

func TestParsePack_RejectsMissingName(t *testing.T) {
	t.Parallel()

	body := []byte(`
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
`)
	_, err := ParsePack(body, "noname.yaml")
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention 'name', got: %v", err)
	}
}

func TestParsePack_RejectsEmptyRules(t *testing.T) {
	t.Parallel()

	body := []byte(`
name: empty
version: 1.0.0
rules: []
`)
	_, err := ParsePack(body, "empty.yaml")
	if err == nil {
		t.Fatal("expected error for empty rules")
	}
	if !strings.Contains(err.Error(), "rules") {
		t.Errorf("error should mention 'rules', got: %v", err)
	}
}

func TestParsePack_RejectsDuplicateRuleIDs(t *testing.T) {
	t.Parallel()

	body := []byte(`
name: dupe
version: 1.0.0
rules:
  - id: same
    regex: 'A+'
  - id: same
    regex: 'B+'
`)
	_, err := ParsePack(body, "dupe.yaml")
	if err == nil {
		t.Fatal("expected error for duplicate rule ids")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention 'duplicate', got: %v", err)
	}
}

func TestParsePack_NameMustMatchFilenameStem(t *testing.T) {
	t.Parallel()

	body := []byte(`
name: not-the-filename
version: 1.0.0
rules:
  - id: x
    regex: 'X+'
`)
	_, err := ParsePack(body, "actual-filename.yaml")
	if err == nil {
		t.Fatal("expected error for name/filename mismatch")
	}
	if !strings.Contains(err.Error(), "name") || !strings.Contains(err.Error(), "filename") {
		t.Errorf("error should mention name/filename, got: %v", err)
	}
}

func TestLoadPacks_ReadsMultipleFilesAndIgnoresOthers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	mustWrite(t, dir, "alpha.yaml", `
name: alpha
version: 1.0.0
rules:
  - id: a
    regex: 'A+'
`)
	mustWrite(t, dir, "beta.json", `{
  "name": "beta",
  "version": "1.0.0",
  "rules": [{"id": "b", "regex": "B+"}]
}`)
	mustWrite(t, dir, "ignored.txt", `not a pack`)
	mustWrite(t, dir, "broken.yaml", `name: [this is malformed`)

	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks: %v", err)
	}
	if len(packs) != 2 {
		t.Fatalf("packs: want 2 (alpha, beta), have %d", len(packs))
	}

	got := map[string]bool{}
	for _, p := range packs {
		got[p.Name] = true
	}
	if !got["alpha"] || !got["beta"] {
		t.Errorf("expected packs alpha+beta, got %v", got)
	}
}

func TestLoadPacks_MissingDirReturnsEmpty(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	packs, err := LoadPacks(dir)
	if err != nil {
		t.Fatalf("LoadPacks should be tolerant of missing dirs, got: %v", err)
	}
	if len(packs) != 0 {
		t.Errorf("packs: want 0, have %d", len(packs))
	}
}

func mustWrite(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
