package redact

import (
	"log/slog"
	"regexp"
	"sync"
)

// CustomRulesConfig is the user-supplied input to ConfigureCustomRules.
//
// Inline entries come from redaction.custom_secrets in settings.json.
// Packs come from .entire/redactors/*.{yaml,yml,json} via LoadPacks.
// Both feed the same compiled rule table.
type CustomRulesConfig struct {
	// Inline maps a label (used only in logs/diagnostics) to a Go RE2 regex
	// string. Failed compilations are logged via slog.Warn and dropped.
	Inline map[string]string

	// Packs are pre-parsed rule packs (see LoadPacks). Per-rule regex
	// compilation failures are logged and dropped; sample mismatches are
	// logged but do not drop the rule.
	Packs []*Pack
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
// result for use by redact.String(). Sample-validation runs here too, so
// failures surface the next time any process initializes redaction.
//
// Call once at process startup after loading settings. Thread-safe.
func ConfigureCustomRules(cfg CustomRulesConfig) {
	state := &customRulesState{}

	// Inline rules.
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

	// Pack rules.
	for _, pack := range cfg.Packs {
		for _, rule := range pack.Rules {
			compiled, err := regexp.Compile(rule.Regex)
			if err != nil {
				slog.Warn("skipping invalid pack rule",
					slog.String("pack", pack.SourcePath),
					slog.String("rule", rule.ID),
					slog.String("error", err.Error()))
				continue
			}
			state.rules = append(state.rules, compiledCustomRule{
				label: pack.Name + "." + rule.ID,
				regex: compiled,
			})
			runRuleSamples(pack, rule, compiled)
		}
	}

	customConfigMu.Lock()
	defer customConfigMu.Unlock()
	customConfig = state
}

// runRuleSamples checks each sample against the compiled regex and logs a
// warning per mismatch. Failures never drop the rule — sample validation
// is informational, not gating.
func runRuleSamples(pack *Pack, rule Rule, compiled *regexp.Regexp) {
	for _, s := range rule.Samples {
		got := compiled.MatchString(s.Input)
		if got != s.Redacted {
			slog.Warn("redactor pack sample mismatch",
				slog.String("pack", pack.SourcePath),
				slog.String("rule", rule.ID),
				slog.String("sample", s.Input),
				slog.Bool("expected", s.Redacted),
				slog.Bool("got", got))
		}
	}
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
