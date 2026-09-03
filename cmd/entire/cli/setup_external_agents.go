package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// discoverNamedExternalAgent resolves one external agent binary by name,
// bypassing the external_agents setting.
//
// Named rather than the full ungated sweep, wherever the caller already knows
// which agent it wants: DiscoverAndRegisterAlways globs every $PATH directory
// and executes every entire-agent-* binary it finds, in repositories that may
// never have opted into external agents at all. The named lookup returns
// immediately for a built-in, so passing an ordinary agent name costs nothing.
//
// The error is deliberately dropped. Every call site reports an unresolvable
// agent a few lines later in its own terms — validateSummaryProvider,
// applyAgentChanges, agent.Get plus printWrongAgentError — and surfacing this
// one instead would replace a message about the agent the user named with one
// about the plugin protocol.
func discoverNamedExternalAgent(ctx context.Context, name types.AgentName) {
	//nolint:errcheck,gosec // see doc comment: the caller reports the failure
	external.DiscoverAndRegisterNamedAlways(ctx, name)
}

// enableExternalAgentsLocally turns external_agents on in
// .entire/settings.local.json.
//
// Always the local file, whatever --local/--project said about the rest of the
// write. external_agents grants execution of entire-agent-* binaries found on
// $PATH, and settings.Load honors that grant only from an untracked local file
// (see settings.enforceExternalAgentsTrust). Writing it to the project file
// would produce a setting the user can read back but that never takes effect:
// the plugin they just chose would silently stop being discovered on the next
// command.
//
// The choice is machine-specific anyway — it depends on what is on this
// developer's $PATH — so the local file is where it belonged before the trust
// gate existed too. persistSummaryProviderSelection already reasoned its way
// to the same place.
//
// Raw read-modify-write, not a struct save: the merged struct carries the
// project file's fields as well, so writing it into the local file would copy
// everyone's settings into this developer's overrides. Same rule as
// setEnabledRaw.
func enableExternalAgentsLocally(ctx context.Context) error {
	path, raw, _, err := settings.LoadLocalRaw(ctx)
	if err != nil {
		return fmt.Errorf("failed to load local settings: %w", err)
	}
	raw["external_agents"] = json.RawMessage("true")
	if err := settings.SaveLocalRaw(path, raw); err != nil {
		return fmt.Errorf("failed to save external_agents setting: %w", err)
	}
	return nil
}
