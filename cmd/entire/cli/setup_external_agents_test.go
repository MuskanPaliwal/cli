package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// The auto-enable that fires when a user picks an external agent has to land
// somewhere the loader will honor. Written into the version-controlled
// .entire/settings.json it is dropped on the next load, so the agent the user
// just chose silently stops being discovered.
func TestEnableExternalAgentsLocally_TakesEffect(t *testing.T) { //nolint:paralleltest // t.Chdir
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	entireDir := filepath.Join(dir, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("create .entire: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.json"),
		[]byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatalf("write project settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"),
		[]byte(`{"log_level":"debug"}`), 0o644); err != nil {
		t.Fatalf("write local settings: %v", err)
	}
	t.Chdir(dir)

	if err := enableExternalAgentsLocally(t.Context()); err != nil {
		t.Fatalf("enableExternalAgentsLocally: %v", err)
	}

	s, err := settings.Load(t.Context())
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if !s.ExternalAgents {
		reason, _ := s.ExternalAgentsRejection()
		t.Errorf("external_agents did not take effect (rejection: %q)", reason)
	}
	if s.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want the existing local setting preserved", s.LogLevel)
	}
}
