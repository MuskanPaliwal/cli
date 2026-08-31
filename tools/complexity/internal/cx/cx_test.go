package cx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes cfg to a temp file and loads it the way the binaries do.
func writeConfig(t *testing.T, cfg Config) *Config {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "features.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return c
}

// A broad glob listed before a rule naming one file exactly must not absorb
// that file. This shape is invisible in the output — the file lands in a
// plausible wrong feature rather than in _unmapped — so it is pinned here
// rather than left to review.
func TestFor_ExactPathBeatsEarlierGlob(t *testing.T) {
	t.Parallel()
	c := writeConfig(t, Config{Features: []FeatureRule{
		{Name: "configure", Area: "command", Paths: []string{"cli/setup*.go"}},
		{Name: "import", Area: "command", Paths: []string{"cli/setup_import.go"}},
	}})

	for _, tc := range []struct{ file, want string }{
		{"cli/setup_import.go", "import"},      // exact rule wins despite order
		{"cli/setup_import_test.go", "import"}, // and its test follows it
		{"cli/setup.go", "configure"},          // glob still claims the rest
		{"cli/setup_github_test.go", "configure"},
	} {
		if got := c.For(tc.file).Name; got != tc.want {
			t.Errorf("For(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// A rule naming a _test.go file explicitly outranks its source's rule.
func TestFor_ExplicitTestRuleWins(t *testing.T) {
	t.Parallel()
	c := writeConfig(t, Config{Features: []FeatureRule{
		{Name: "feature", Area: "a", Paths: []string{"cli/thing.go"}},
		{Name: "test-infra", Area: "t", Paths: []string{"cli/thing_test.go"}, NoRank: true},
	}})
	if got := c.For("cli/thing_test.go"); got.Name != "test-infra" || got.Ranked {
		t.Errorf("For(thing_test.go) = %+v, want test-infra with Ranked=false", got)
	}
	if got := c.For("cli/thing.go").Name; got != "feature" {
		t.Errorf("For(thing.go) = %q, want feature", got)
	}
}

// Globs never cross a directory boundary, and a trailing "/" is a prefix rule.
func TestFor_GlobDoesNotCrossDirectories(t *testing.T) {
	t.Parallel()
	c := writeConfig(t, Config{Features: []FeatureRule{
		{Name: "top", Area: "a", Paths: []string{"cmd/*.go"}},
		{Name: "tree", Area: "b", Paths: []string{"cmd/deep/"}},
	}})
	if got := c.For("cmd/main.go").Name; got != "top" {
		t.Errorf("cmd/main.go = %q, want top", got)
	}
	if got := c.For("cmd/deep/x.go").Name; got != "tree" {
		t.Errorf("cmd/deep/x.go = %q, want tree", got)
	}
	if got := c.For("other/x.go").Name; got != unmapped.Name {
		t.Errorf("other/x.go = %q, want %q", got, unmapped.Name)
	}
}

// matchRule discards path.Match's error, so an unparseable pattern would match
// nothing while looking correct in the config. LoadConfig must reject it.
func TestLoadConfig_RejectsBadPattern(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Config{Features: []FeatureRule{
		{Name: "broken", Area: "a", Paths: []string{"cli/setup[.go"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "features.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(p)
	if err == nil {
		t.Fatal("LoadConfig accepted a malformed pattern")
	}
	for _, want := range []string{"broken", "cli/setup[.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}
