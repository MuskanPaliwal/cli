// Package cx holds what the cxtool binaries (., ./reach, ./dupl) must agree
// on: the feature-mapping rules, the rank policy, and a few I/O helpers.
// There is exactly one definition of "which feature does this file belong to" —
// keep it that way.
package cx

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

// FeatureRule maps path patterns to a feature. A pattern ending in "/" is a
// directory-prefix match; otherwise the directory must match exactly and the
// basename is a glob. NoRank marks features that are excluded from rankings,
// hotspot tables and headline totals (test infrastructure, generated code,
// test-only agents) while still being measured.
type FeatureRule struct {
	Name   string   `json:"name"`
	Area   string   `json:"area"`
	Paths  []string `json:"paths"`
	NoRank bool     `json:"norank,omitempty"`
	Note   string   `json:"note,omitempty"`
}

type Config struct {
	Doc      string        `json:"_doc"`
	Features []FeatureRule `json:"features"`
}

// LoadConfig reads the feature mapping. A missing file is an error: every
// binary in this module is useless without the mapping.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("features config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("features config: %w", err)
	}
	return &c, nil
}

func matchRule(pattern, rel string) bool {
	if strings.HasSuffix(pattern, "/") {
		return strings.HasPrefix(rel, pattern)
	}
	pdir, pbase := path.Split(pattern)
	rdir, rbase := path.Split(rel)
	if pdir != rdir {
		return false
	}
	ok, _ := path.Match(pbase, rbase)
	return ok
}

// Feature is the resolved mapping for one file.
type Feature struct {
	Name   string
	Area   string
	Ranked bool
}

var unmapped = Feature{Name: "_unmapped", Area: "_unmapped", Ranked: false}

// For maps a repo-relative file path to its feature. Two ordered passes keep
// the "first match wins" contract predictable: the literal path is tried
// against every rule first (so a rule that names a _test.go file explicitly
// always beats a derived match), then, for test files, the source-file name
// (foo_test.go -> foo.go) is tried so tests follow their source by default.
func (c *Config) For(rel string) Feature {
	if f, ok := c.match(rel); ok {
		return f
	}
	if strings.HasSuffix(rel, "_test.go") {
		if f, ok := c.match(strings.TrimSuffix(rel, "_test.go") + ".go"); ok {
			return f
		}
	}
	return unmapped
}

func (c *Config) match(rel string) (Feature, bool) {
	for i := range c.Features {
		r := &c.Features[i]
		for _, p := range r.Paths {
			if matchRule(p, rel) {
				return Feature{Name: r.Name, Area: r.Area, Ranked: !r.NoRank}, true
			}
		}
	}
	return Feature{}, false
}

// ReadModulePath returns the module path of the Go module rooted at dir.
func ReadModulePath(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	mp := modfile.ModulePath(b)
	if mp == "" {
		return "", fmt.Errorf("no module path in %s/go.mod", dir)
	}
	return mp, nil
}

// Check exits with the error on stderr; these are batch CLIs, not libraries.
func Check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// WriteCSV writes one CSV file whole; writers stay pure row-builders.
func WriteCSV(path string, header []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	if err := w.Write(header); err != nil {
		f.Close()
		return err
	}
	if err := w.WriteAll(rows); err != nil {
		f.Close()
		return err
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
