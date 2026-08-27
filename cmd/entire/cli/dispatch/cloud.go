package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

type CloudConfig struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	Timeout time.Duration
}

type CloudClient struct {
	baseURL string
	token   string
	http    *http.Client
}

const defaultCloudHTTPTimeout = 120 * time.Second

// dispatchGeneratePath is the gateway's one-shot dispatch route: the gateway
// generates the markdown in-request and persists nothing, which is what the
// CLI wants (a dispatch on the terminal is a one-off, unlike the web app's saved
// runs on /me/dispatches).
//
// A dispatch covers repos placed in exactly one jurisdiction and is generated
// from that jurisdiction's cell — not the caller's home cell on their behalf.
// The gateway picks the cell from the gateway-only `?jurisdiction=` query
// selector (validated and stripped there; the body never carries it). Omitted
// → the caller's home jurisdiction, so a CLI that sends nothing behaves exactly
// as before.
const (
	dispatchGeneratePath   = "/api/v1/dispatches/generate"
	jurisdictionQueryParam = "jurisdiction"
)

func NewCloudClient(cfg CloudConfig) *CloudClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = api.BaseURL()
	}

	httpClient := cfg.HTTP
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = defaultCloudHTTPTimeout
		}
		httpClient = &http.Client{Timeout: timeout}
	} else if cfg.Timeout > 0 && httpClient.Timeout == 0 {
		httpClient.Timeout = cfg.Timeout
	}

	return &CloudClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   cfg.Token,
		http:    httpClient,
	}
}

type CreateDispatchRequest struct {
	Repos    []string `json:"repos,omitempty"`
	Since    string   `json:"since"`
	Until    string   `json:"until"`
	Generate bool     `json:"generate"`
	Voice    string   `json:"voice,omitempty"`
}

type CreateDispatchResponse struct {
	// Jurisdiction is the slug the gateway stamps onto the response: the
	// jurisdiction whose cell the dispatch was generated from. Empty from a
	// gateway that predates stamping.
	Jurisdiction      string      `json:"jurisdiction,omitempty"`
	Window            APIWindow   `json:"window"`
	Title             string      `json:"title,omitempty"`
	CoveredRepos      []string    `json:"covered_repos,omitempty"`
	Branches          APIBranches `json:"branches,omitempty"`
	Voice             *string     `json:"voice"`
	Repos             []APIRepo   `json:"repos,omitempty"`
	Totals            APITotals   `json:"totals"`
	Warnings          APIWarnings `json:"warnings"`
	GeneratedText     string      `json:"generated_text,omitempty"`
	GeneratedMarkdown string      `json:"generated_markdown,omitempty"`
}

type APIBranches struct {
	Values []string
	All    bool
}

type APIWindow struct {
	NormalizedSince          string `json:"normalized_since"`
	NormalizedUntil          string `json:"normalized_until"`
	FirstCheckpointCreatedAt string `json:"first_checkpoint_created_at,omitempty"`
	LastCheckpointCreatedAt  string `json:"last_checkpoint_created_at,omitempty"`
}

type APIRepo struct {
	FullName string       `json:"full_name"`
	URL      string       `json:"url,omitempty"`
	Sections []APISection `json:"sections"`
}

type APISection struct {
	Label   string      `json:"label"`
	Bullets []APIBullet `json:"bullets"`
}

type APIBullet struct {
	CheckpointID string   `json:"checkpoint_id"`
	Text         string   `json:"text"`
	Source       string   `json:"source"`
	Branch       string   `json:"branch"`
	CreatedAt    string   `json:"created_at"`
	Labels       []string `json:"labels"`
}

type APITotals struct {
	Checkpoints         int `json:"checkpoints"`
	UsedCheckpointCount int `json:"used_checkpoint_count"`
	Branches            int `json:"branches"`
	FilesTouched        int `json:"files_touched"`
}

type APIWarnings struct {
	AccessDeniedCount  int `json:"access_denied_count"`
	PendingCount       int `json:"pending_count"`
	FailedCount        int `json:"failed_count"`
	UnknownCount       int `json:"unknown_count"`
	UncategorizedCount int `json:"uncategorized_count"`
	TruncatedCount     int `json:"truncated_count"`
}

func (b *APIBranches) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*b = APIBranches{}
		return nil
	}

	var values []string
	if err := json.Unmarshal(data, &values); err == nil {
		*b = APIBranches{Values: values}
		return nil
	}

	var sentinel string
	if err := json.Unmarshal(data, &sentinel); err != nil {
		return fmt.Errorf("decode branches: %w", err)
	}
	if sentinel != "all" {
		return fmt.Errorf("decode branches: unexpected sentinel %q", sentinel)
	}
	*b = APIBranches{All: true}
	return nil
}

// RepoNotFoundError is the gateway's 404 for a repo that is not placed in (or
// not visible in) the jurisdiction the request was routed to. Jurisdiction is
// the selector the caller sent ("" = home); Repos are the slugs the gateway
// named, when its message listed them.
//
// This is the cross-jurisdiction failure the selector exists for: a repo
// mirrored only in US, requested from an AU home, is simply unknown to the AU
// cell. The message therefore says which jurisdiction was targeted and how to
// pick another, rather than reading like the repo does not exist at all.
type RepoNotFoundError struct {
	Jurisdiction string
	Repos        []string
	Message      string
}

const repoNotFoundPrefix = "repository not found"

func (e *RepoNotFoundError) Error() string {
	return fmt.Sprintf(
		"In %s: %s. Pick a jurisdiction the repository is mirrored into (entire dispatch --jurisdiction <slug>), or mirror it there.",
		e.ScopeLabel(), e.Message,
	)
}

// ScopeLabel names the jurisdiction the failed request targeted, the way the
// web app does ("US"), or the home default when none was selected.
func (e *RepoNotFoundError) ScopeLabel() string {
	if strings.TrimSpace(e.Jurisdiction) == "" {
		return "your home jurisdiction"
	}
	return strings.ToUpper(e.Jurisdiction)
}

// httpStatusError is a non-2xx answer with whatever message the body carried
// (gateway `{"error"}` or huma `{"detail"}` shape), so callers can branch on
// status without re-parsing. Its text is the generic user-facing form.
type httpStatusError struct {
	status  int
	message string
	raw     string
}

func (e *httpStatusError) Error() string {
	if e.raw == "" {
		return fmt.Sprintf("dispatch service returned status %d", e.status)
	}
	return fmt.Sprintf("dispatch service returned status %d: %s", e.status, strconv.Quote(e.raw))
}

// CreateDispatch generates a one-off dispatch from the cell of `jurisdiction`
// ("" = the caller's home jurisdiction).
func (c *CloudClient) CreateDispatch(ctx context.Context, reqBody CreateDispatchRequest, jurisdiction string) (*CreateDispatchResponse, error) {
	var out CreateDispatchResponse
	err := c.doJSON(ctx, http.MethodPost, dispatchGeneratePath, jurisdictionQuery(jurisdiction), reqBody, &out)
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound &&
			strings.HasPrefix(strings.ToLower(statusErr.message), repoNotFoundPrefix) {
			return nil, &RepoNotFoundError{
				Jurisdiction: jurisdiction,
				Repos:        parseNotFoundRepos(statusErr.message),
				Message:      statusErr.message,
			}
		}
		return nil, err
	}
	if out.Jurisdiction == "" {
		// A gateway that does not stamp: the row lives where we asked.
		out.Jurisdiction = strings.TrimSpace(jurisdiction)
	}
	return &out, nil
}

func jurisdictionQuery(jurisdiction string) url.Values {
	j := strings.TrimSpace(jurisdiction)
	if j == "" {
		return nil
	}
	return url.Values{jurisdictionQueryParam: []string{j}}
}

// parseNotFoundRepos pulls the slugs out of a "repository not found: a/b, c/d"
// (or "...in its region: a/b") message. Best-effort: an unexpected format
// yields nil and the caller still has the message.
func parseNotFoundRepos(message string) []string {
	_, rest, ok := strings.Cut(message, ":")
	if !ok {
		return nil
	}
	var repos []string
	for _, part := range strings.Split(rest, ",") {
		if p := strings.TrimSpace(part); p != "" {
			repos = append(repos, p)
		}
	}
	return repos
}

// errorMessageFromBody extracts the human message from an error body in
// either the gateway's `{"error": "..."}` or huma's `{"detail": "..."}` shape.
// Non-JSON bodies yield "".
func errorMessageFromBody(body string) string {
	if body == "" {
		return ""
	}
	var parsed struct {
		Error   string `json:"error"`
		Detail  string `json:"detail"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return ""
	}
	for _, candidate := range []string{parsed.Error, parsed.Detail, parsed.Message} {
		if c := strings.TrimSpace(candidate); c != "" {
			return c
		}
	}
	return ""
}

func (c *CloudClient) doJSON(ctx context.Context, method, path string, query url.Values, reqBody, out any) error {
	var body io.Reader
	if reqBody != nil {
		data, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		body = bytes.NewReader(data)
	}

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", versioninfo.UserAgent())
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("dispatch requires login — run `entire login`")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck // best-effort body read for error message
		trimmed := strings.TrimSpace(string(raw))
		logging.Warn(ctx, "dispatch request failed", "method", method, "path", path, "status_code", resp.StatusCode)
		return &httpStatusError{status: resp.StatusCode, message: errorMessageFromBody(trimmed), raw: trimmed}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
