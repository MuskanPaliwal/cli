package redact

import (
	"strings"
	"sync"
	"testing"
)

// resetCustomRulesForTest clears the package-level config so tests don't leak
// state into each other. Tests cannot run in parallel against the global, so
// the helper is invoked at top-of-test in a t.Cleanup pattern.
func resetCustomRulesForTest(t *testing.T) {
	t.Helper()
	customConfigMu.Lock()
	customConfig = nil
	customConfigMu.Unlock()
	t.Cleanup(func() {
		customConfigMu.Lock()
		customConfig = nil
		customConfigMu.Unlock()
	})
}

func TestConfigureCustomRules_CompilesAndStoresRules(t *testing.T) {
	resetCustomRulesForTest(t)

	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{
			"acme_token": `ACME_TOKEN_[A-Za-z0-9]{20,}`,
		},
	})

	cfg := getCustomRulesConfig()
	if cfg == nil {
		t.Fatal("expected config, got nil")
	}
	if len(cfg.rules) != 1 {
		t.Fatalf("rules: want 1, have %d", len(cfg.rules))
	}
	if cfg.rules[0].label != "acme_token" {
		t.Errorf("label: want acme_token, have %q", cfg.rules[0].label)
	}
	if cfg.rules[0].regex == nil {
		t.Fatal("regex is nil")
	}
	if !cfg.rules[0].regex.MatchString("ACME_TOKEN_abc123def456ghi789jkl") {
		t.Error("compiled regex should match a known sample")
	}
}

func TestConfigureCustomRules_SkipsInvalidRegexAndContinues(t *testing.T) {
	resetCustomRulesForTest(t)

	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{
			"valid":   `[A-Z]{8}`,
			"invalid": `[unterminated`,
		},
	})

	cfg := getCustomRulesConfig()
	if cfg == nil {
		t.Fatal("expected config")
	}
	if got := len(cfg.rules); got != 1 {
		t.Fatalf("rules: want 1 (invalid dropped), have %d", got)
	}
	if cfg.rules[0].label != "valid" {
		t.Errorf("label: want valid, have %q", cfg.rules[0].label)
	}
}

func TestConfigureCustomRules_EmptyConfigStoresNoRules(t *testing.T) {
	resetCustomRulesForTest(t)

	ConfigureCustomRules(CustomRulesConfig{})

	cfg := getCustomRulesConfig()
	if cfg == nil {
		t.Fatal("expected config")
	}
	if len(cfg.rules) != 0 {
		t.Errorf("rules: want 0, have %d", len(cfg.rules))
	}
}

func TestConfigureCustomRules_ConcurrentReadsAndWrites(t *testing.T) {
	resetCustomRulesForTest(t)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			ConfigureCustomRules(CustomRulesConfig{Inline: map[string]string{"x": `[a-z]+`}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_ = getCustomRulesConfig()
		}
	}()
	wg.Wait()
}

func TestDetectCustomRules_NoConfigReturnsNil(t *testing.T) {
	resetCustomRulesForTest(t)

	if got := detectCustomRules(nil, "ACME_TOKEN_abc123def456ghi789jkl"); got != nil {
		t.Errorf("nil config: want nil, have %v", got)
	}
}

func TestDetectCustomRules_SingleRuleMultipleMatches(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{"token": `T_[a-z]{6}`},
	})

	input := "first T_aaaaaa middle T_bbbbbb last"
	regions := detectCustomRules(getCustomRulesConfig(), input)
	if len(regions) != 2 {
		t.Fatalf("regions: want 2, have %d (%v)", len(regions), regions)
	}
	for i, r := range regions {
		if r.label != "" {
			t.Errorf("region %d: label want empty (bare REDACTED), have %q", i, r.label)
		}
	}
}

func TestDetectCustomRules_NonMatchingInputReturnsEmpty(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{"token": `T_[a-z]{6}`},
	})

	got := detectCustomRules(getCustomRulesConfig(), "no matches here at all")
	if len(got) != 0 {
		t.Errorf("regions: want 0, have %d", len(got))
	}
}

func TestString_CustomRuleEndToEnd(t *testing.T) {
	resetCustomRulesForTest(t)
	ConfigureCustomRules(CustomRulesConfig{
		Inline: map[string]string{
			"acme_token": `ACME_TOKEN_[A-Za-z0-9]{20,}`,
		},
	})

	in := "key=ACME_TOKEN_abc123def456ghi789jkl after"
	out := String(in)

	if !contains(t, out, "REDACTED") {
		t.Errorf("expected REDACTED in output, got: %q", out)
	}
	if contains(t, out, "ACME_TOKEN_abc123def456ghi789jkl") {
		t.Errorf("raw token leaked into output: %q", out)
	}
	if contains(t, out, "[REDACTED_") {
		t.Errorf("custom rule used PII-style token: %q", out)
	}
}

func TestString_CustomRuleNotConfiguredIsNoop(t *testing.T) {
	resetCustomRulesForTest(t)

	in := "T_aaaaaa T_bbbbbb"
	if got := String(in); got != in {
		t.Errorf("expected unchanged %q, got %q", in, got)
	}
}

func contains(t *testing.T, s, sub string) bool {
	t.Helper()
	return strings.Contains(s, sub)
}
