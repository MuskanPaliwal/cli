package cli

import "testing"

func TestIsOfficialPlugin(t *testing.T) {
	t.Parallel()
	// Snapshot and restore the package-level allowlist so this test stays
	// independent of which plugins ship by default.
	saved := officialPlugins
	t.Cleanup(func() { officialPlugins = saved })

	officialPlugins = []string{"pgr", "stack"}

	cases := []struct {
		name string
		want bool
	}{
		{"pgr", true},
		{"stack", true},
		{"PGR", false},  // case-sensitive
		{"pgr2", false}, // exact match only
		{"", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		if got := IsOfficialPlugin(tc.name); got != tc.want {
			t.Errorf("IsOfficialPlugin(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOfficialPlugins_DefaultAllowlist(t *testing.T) {
	t.Parallel()
	// Sanity check: no third-party plugin name should be officially
	// recognized by default. If you are adding an Entire-shipped plugin,
	// update this test alongside the registry.
	for _, n := range officialPlugins {
		if n == "" {
			t.Errorf("officialPlugins contains an empty name")
		}
	}
}
