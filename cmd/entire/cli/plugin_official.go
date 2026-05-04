package cli

// Official plugin allowlist. Names listed here may contribute their plugin
// name to telemetry; all other (third-party / user-installed) plugins are
// invoked silently with no event recorded. The reasoning mirrors gh's
// extension policy — third-party plugin names can carry sensitive identifiers
// (project names, vendor names), so we only attribute usage for plugins we
// ship ourselves.
//
// To add a plugin: append its name to officialPlugins. Match must be exact
// and case-sensitive. The corresponding binary is `entire-<name>`.
//
//nolint:gochecknoglobals // small immutable allowlist
var officialPlugins = []string{
	// Add Entire-shipped plugin names here as they're released.
	// e.g. "pgr"
}

// IsOfficialPlugin reports whether name appears in the hardcoded allowlist.
// Used to decide whether plugin invocation telemetry should record the name.
func IsOfficialPlugin(name string) bool {
	for _, p := range officialPlugins {
		if p == name {
			return true
		}
	}
	return false
}
