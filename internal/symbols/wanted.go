package symbols

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// WantedPath lists the symbol files crashes needed and did not find;
// WantedPageSize is the endpoint's maximum page.
const (
	WantedPath     = "/api/v1/symbols/wanted"
	WantedPageSize = 200
)

// WantedSymbol is one identity a crash needed, as an upload would carry it;
// AvailableSince is set once a matching upload arrived.
type WantedSymbol struct {
	AppKey          string  `json:"app_key"`
	Platform        string  `json:"platform"`
	Format          string  `json:"format"`
	SymbolID        string  `json:"symbol_id"`
	FirstSeenAt     string  `json:"first_seen_at"`
	LastSeenAt      string  `json:"last_seen_at"`
	OccurrenceCount int64   `json:"occurrence_count"`
	AvailableSince  *string `json:"available_since"`
}

// WantedPage is one page of the listing; Next is empty on the last page.
type WantedPage struct {
	Wanted []WantedSymbol `json:"wanted"`
	Total  int64          `json:"total"`
	Next   string         `json:"next"`
}

// Lister reads the listing with bearer auth, retrying only 5xx and transport
// failures, like Uploader. Fill BaseURL and APIKey.
type Lister struct {
	BaseURL   string
	APIKey    string
	Client    *http.Client
	Retries   int
	Backoff   func(retry int) time.Duration
	UserAgent string
	// SDKIdentity is the CLI's validated request identity.
	SDKIdentity string
}

// Page reads the page at cursor ("" for the first); non-2xx is an *APIError.
func (l *Lister) Page(ctx context.Context, cursor string) (WantedPage, error) {
	backoff := l.Backoff
	if backoff == nil {
		backoff = defaultBackoff
	}
	for attempt := 0; ; attempt++ {
		page, err := l.attempt(ctx, cursor)
		var apiErr *APIError
		if err == nil || (errors.As(err, &apiErr) && !apiErr.Retryable()) || attempt >= max(l.Retries, 0) {
			return page, err
		}
		select {
		case <-time.After(backoff(attempt)):
		case <-ctx.Done():
			return WantedPage{}, fmt.Errorf("canceled while waiting to retry: %w (last error: %v)", ctx.Err(), err)
		}
	}
}

func (l *Lister) attempt(ctx context.Context, cursor string) (WantedPage, error) {
	query := url.Values{"limit": {strconv.Itoa(WantedPageSize)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+WantedPath+"?"+query.Encode(), nil)
	if err != nil {
		return WantedPage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+l.APIKey)
	req.Header.Set("Accept", "application/json")
	if l.UserAgent != "" {
		req.Header.Set("User-Agent", l.UserAgent)
	}
	if l.SDKIdentity != "" {
		req.Header.Set("X-ShardPilot-SDK", l.SDKIdentity)
	}
	client := l.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return WantedPage{}, fmt.Errorf("get %s: %w", WantedPath, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	if err != nil {
		return WantedPage{}, fmt.Errorf("read endpoint response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return WantedPage{}, newAPIError(resp.StatusCode, body)
	}
	var page WantedPage
	if err := json.Unmarshal(body, &page); err != nil {
		return WantedPage{}, fmt.Errorf("unreadable %s response (http 200): %w", WantedPath, err)
	}
	return page, nil
}
