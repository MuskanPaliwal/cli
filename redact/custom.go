package redact

import (
	"log/slog"
	"regexp"
	"sync"
)

// CustomRulesConfig is the user-supplied input to ConfigureCustomRules.
//
// Inline entries come from redaction.custom_secrets in settings.json. Packs
// are added in a follow-up change; both feed the same compiled rule table.
type CustomRulesConfig struct {
	// Inline maps a label (used only in logs/diagnostics) to a Go RE2 regex
	// string. Failed compilations are logged via slog.Warn and dropped.
	Inline map[string]string
}

// compiledCustomRule is a compiled regex retained across calls.
// label is unused for replacement (custom rules always emit the bare REDACTED
// token to match other secret layers), but is preserved for diagnostics.
type compiledCustomRule struct {
	label string
	regex *regexp.Regexp
}

// customRulesState is the package-level table read by detectCustomRules.
type customRulesState struct {
	rules []compiledCustomRule
}

var (
	customConfig   *customRulesState
	customConfigMu sync.RWMutex
)

// ConfigureCustomRules compiles user-defined redaction rules and stores the
// result for use by redact.String(). Call once at process startup after
// loading settings. Thread-safe.
func ConfigureCustomRules(cfg CustomRulesConfig) {
	state := &customRulesState{}
	for label, pattern := range cfg.Inline {
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			slog.Warn("skipping invalid custom_secrets pattern",
				slog.String("label", label),
				slog.String("error", err.Error()))
			continue
		}
		state.rules = append(state.rules, compiledCustomRule{label: label, regex: compiled})
	}

	customConfigMu.Lock()
	defer customConfigMu.Unlock()
	customConfig = state
}

// getCustomRulesConfig returns the currently-configured custom rules.
// Returns nil if ConfigureCustomRules has never been called.
func getCustomRulesConfig() *customRulesState {
	customConfigMu.RLock()
	defer customConfigMu.RUnlock()
	return customConfig
}

// detectCustomRules returns tagged regions for every match of every
// configured custom rule. Returns nil if no rules are configured.
//
// All regions use an empty label so they are replaced with the bare
// "REDACTED" token used by the built-in secret layers, not the
// "[REDACTED_<LABEL>]" token used by PII.
func detectCustomRules(cfg *customRulesState, s string) []taggedRegion {
	if cfg == nil || len(cfg.rules) == 0 {
		return nil
	}
	var regions []taggedRegion
	for _, rule := range cfg.rules {
		for _, loc := range rule.regex.FindAllStringIndex(s, -1) {
			regions = append(regions, taggedRegion{region: region{loc[0], loc[1]}})
		}
	}
	return regions
}
