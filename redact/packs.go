package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// RedactorsDirName is the .entire subdirectory used for user-defined rule packs.
const RedactorsDirName = "redactors"

// Pack is a versioned bundle of redaction rules loaded from a single file
// under .entire/redactors/. Both YAML and JSON encodings are accepted; the
// schema is identical.
type Pack struct {
	Name        string `json:"name"                  yaml:"name"`
	Version     string `json:"version"               yaml:"version"`
	Description string `json:"description,omitempty" yaml:"description"`
	Rules       []Rule `json:"rules"                 yaml:"rules"`

	// sourcePath is the file the pack was loaded from. Populated by
	// ParsePack; used in log lines so users can find the offending file.
	sourcePath string
}

// Rule is a single redaction rule within a Pack.
type Rule struct {
	ID          string   `json:"id"                    yaml:"id"`
	Description string   `json:"description,omitempty" yaml:"description"`
	Regex       string   `json:"regex"                 yaml:"regex"`
	Samples     []Sample `json:"samples,omitempty"     yaml:"samples"`
}

// Sample is a self-test entry for a Rule. The runner asserts whether the
// rule's regex matching `Input` matches the `Redacted` expectation.
type Sample struct {
	Input    string `json:"input"    yaml:"input"`
	Redacted bool   `json:"redacted" yaml:"redacted"`
}

// ParsePack decodes a single pack file. sourcePath is used both to pick
// the encoding (YAML by default; JSON only when the extension is .json)
// and to enforce that the pack's `name` matches the filename stem.
func ParsePack(data []byte, sourcePath string) (*Pack, error) {
	var pack Pack
	switch strings.ToLower(filepath.Ext(sourcePath)) {
	case ".json":
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&pack); err != nil {
			return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
		}
	default:
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&pack); err != nil {
			return nil, fmt.Errorf("parse %s: %w", sourcePath, err)
		}
	}
	pack.sourcePath = sourcePath
	if err := validatePack(&pack); err != nil {
		return nil, err
	}
	return &pack, nil
}

// validatePack enforces required fields and schema invariants. Regex
// compilation is NOT performed here — that happens during ConfigureCustomRules
// so failures emit the same warn-and-skip path as inline rules.
func validatePack(p *Pack) error {
	if p.Name == "" {
		return fmt.Errorf("%s: missing required field 'name'", p.sourcePath)
	}
	if p.Version == "" {
		return fmt.Errorf("%s: missing required field 'version'", p.sourcePath)
	}
	if len(p.Rules) == 0 {
		return fmt.Errorf("%s: 'rules' must contain at least one entry", p.sourcePath)
	}

	// Name must match filename stem so log lines and discovery stay consistent.
	stem := strings.TrimSuffix(filepath.Base(p.sourcePath), filepath.Ext(p.sourcePath))
	if stem != p.Name {
		return fmt.Errorf("%s: pack name %q does not match filename stem %q", p.sourcePath, p.Name, stem)
	}

	seen := make(map[string]struct{}, len(p.Rules))
	for i, r := range p.Rules {
		if r.ID == "" {
			return fmt.Errorf("%s: rules[%d] missing required field 'id'", p.sourcePath, i)
		}
		if r.Regex == "" {
			return fmt.Errorf("%s: rules[%d] (%s) missing required field 'regex'", p.sourcePath, i, r.ID)
		}
		if _, dup := seen[r.ID]; dup {
			return fmt.Errorf("%s: duplicate rule id %q", p.sourcePath, r.ID)
		}
		seen[r.ID] = struct{}{}
	}
	return nil
}

// LoadPacks discovers and parses all rule packs in dir, including any
// subdirectories (so the conventional .entire/redactors/local/ path for
// personal/uncommitted rules is picked up automatically). Files with the
// extensions .yaml, .yml, and .json are considered packs; other files are
// ignored. A missing directory is treated as "no packs configured" and
// returns no error. Per-file parse errors are slog.Warn'd and the file is
// skipped — never fatal — so one bad file does not silence the rest.
func LoadPacks(dir string) ([]*Pack, error) {
	var packs []*Pack
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				if os.IsNotExist(err) {
					return nil
				}
				return fmt.Errorf("read redactors dir %s: %w", dir, err)
			}
			slog.Warn("skipping unreadable redactor pack path",
				slog.String("path", path),
				slog.String("error", err.Error()))
			return nil
		}
		if path == dir && !d.IsDir() {
			return fmt.Errorf("redactors path %s is not a directory", dir)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			slog.Warn("skipping symlinked redactor pack path", slog.String("path", path))
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(d.Name())) {
		case ".yaml", ".yml", ".json":
		default:
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir under a configured dir
		if err != nil {
			slog.Warn("skipping unreadable redactor pack",
				slog.String("path", path),
				slog.String("error", err.Error()))
			return nil
		}
		pack, err := ParsePack(data, path)
		if err != nil {
			slog.Warn("skipping invalid redactor pack",
				slog.String("path", path),
				slog.String("error", err.Error()))
			return nil
		}
		packs = append(packs, pack)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk redactors dir %s: %w", dir, walkErr)
	}
	return packs, nil
}
