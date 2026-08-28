package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// requireSecureDispatchURL is the secure-base-URL guard used before the cloud
// client sends a bearer token. Tests swap it to allow httptest.NewServer
// (http://127.0.0.1:...) endpoints; production always routes through
// api.RequireSecureURL and rejects plain HTTP.
var requireSecureDispatchURL = api.RequireSecureURL

func runServer(ctx context.Context, opts Options) (*Dispatch, error) {
	baseURL := api.BaseURL()
	if opts.InsecureHTTPAuth {
		auth.EnableInsecureHTTP()
	} else {
		if err := requireSecureDispatchURL(baseURL); err != nil {
			return nil, fmt.Errorf("dispatch base URL: %w", err)
		}
	}

	// OriginOnly strips any path the operator may have included in
	// ENTIRE_API_BASE_URL — discovery keys off the host.
	token, err := lookupResourceToken(ctx, api.OriginOnly(baseURL))
	if errors.Is(err, auth.ErrNotLoggedIn) {
		return nil, errors.New("dispatch requires login — run `entire login`")
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials: %w", err)
	}

	now := nowUTC()
	sinceInput := strings.TrimSpace(opts.Since)
	if sinceInput == "" {
		sinceInput = "7d"
	}
	since, err := ParseSinceAtNow(sinceInput, now)
	if err != nil {
		return nil, err
	}
	until, err := ParseUntilAtNow(opts.Until, now)
	if err != nil {
		return nil, err
	}
	normalizedSince, normalizedUntil := NormalizeWindow(since, until)
	if !normalizedSince.Before(normalizedUntil) {
		return nil, errors.New("--since must be before --until")
	}

	repos := append([]string(nil), opts.RepoPaths...)
	if len(repos) == 0 {
		repoRoot, err := paths.WorktreeRoot(ctx)
		if err != nil {
			return nil, fmt.Errorf("not in a git repository: %w", err)
		}
		repo, err := gitrepo.OpenPath(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("open repository: %w", err)
		}
		defer repo.Close()

		repoFullName, err := resolveRepoFullName(ctx, repo)
		if err != nil {
			return nil, err
		}
		repos = []string{repoFullName}
	}

	cloud := NewCloudClient(CloudConfig{BaseURL: baseURL, Token: token})
	reqBody := CreateDispatchRequest{
		Repos:    repos,
		Since:    normalizedSince.Format(time.RFC3339),
		Until:    normalizedUntil.Format(time.RFC3339),
		Generate: true,
		Voice:    resolvedDispatchVoicePreference(opts.Voice),
	}
	response, err := cloud.CreateDispatch(ctx, reqBody, opts.Jurisdiction)
	if err != nil {
		return nil, err
	}
	if err := checkDispatchJurisdiction(ctx, opts.Jurisdiction, response.Jurisdiction); err != nil {
		return nil, err
	}

	dispatch := apiToDispatch(response)
	if strings.TrimSpace(dispatch.GeneratedText) == "" {
		return nil, errDispatchMissingMarkdown
	}
	return dispatch, nil
}

// checkDispatchJurisdiction verifies the gateway generated the dispatch where
// --jurisdiction asked. A different stamp is a wrong-region result and fails
// (worse than no result: the output would be labelled with a jurisdiction it
// did not come from). No stamp at all means a gateway that predates the
// selector and ignored it — reported at warn level only, since older gateways
// are expected during rollout and the home-routed output is still a valid
// dispatch of the caller's own repos.
func checkDispatchJurisdiction(ctx context.Context, requested, stamped string) error {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil
	}
	stamped = strings.ToLower(strings.TrimSpace(stamped))
	switch {
	case stamped == "":
		logging.Warn(ctx, "dispatch service did not confirm the requested jurisdiction; it may predate --jurisdiction", "requested", requested)
	case stamped != requested:
		return fmt.Errorf("dispatch was generated in jurisdiction %s, not the requested %s", strings.ToUpper(stamped), strings.ToUpper(requested))
	}
	return nil
}

func apiToDispatch(response *CreateDispatchResponse) *Dispatch {
	if response == nil {
		return &Dispatch{}
	}

	repos := make([]RepoGroup, 0, len(response.Repos))
	for _, repo := range response.Repos {
		sections := make([]Section, 0, len(repo.Sections))
		for _, section := range repo.Sections {
			bullets := make([]Bullet, 0, len(section.Bullets))
			for _, bullet := range section.Bullets {
				bullets = append(bullets, Bullet{
					CheckpointID: bullet.CheckpointID,
					Text:         bullet.Text,
					Source:       bullet.Source,
					Branch:       bullet.Branch,
					CreatedAt:    parseAPITime(bullet.CreatedAt),
					Labels:       append([]string(nil), bullet.Labels...),
				})
			}
			sections = append(sections, Section{
				Label:   section.Label,
				Bullets: bullets,
			})
		}
		repos = append(repos, RepoGroup{
			FullName: repo.FullName,
			URL:      githubRepoURL(repo.FullName),
			Sections: sections,
		})
	}

	generatedText := strings.TrimSpace(response.GeneratedMarkdown)
	if generatedText == "" {
		generatedText = strings.TrimSpace(response.GeneratedText)
	}

	return &Dispatch{
		Window: Window{
			NormalizedSince:   parseAPITime(response.Window.NormalizedSince),
			NormalizedUntil:   parseAPITime(response.Window.NormalizedUntil),
			FirstCheckpointAt: parseAPITime(response.Window.FirstCheckpointCreatedAt),
			LastCheckpointAt:  parseAPITime(response.Window.LastCheckpointCreatedAt),
		},
		CoveredRepos:  append([]string(nil), response.CoveredRepos...),
		Repos:         repos,
		GeneratedText: generatedText,
	}
}

func parseAPITime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}
