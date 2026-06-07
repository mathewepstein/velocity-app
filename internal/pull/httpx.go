package pull

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/mathewepstein/velocity/internal/progress"
)

// httpDoer is satisfied by *http.Client; interface lets tests swap in a fake.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// backoffClient wraps an httpDoer with exponential backoff on 429 / 5xx and
// respects Retry-After when present. Shared between the Jira and GitHub
// pullers because the retry behavior is the same for both APIs.
type backoffClient struct {
	inner    httpDoer
	maxTries int
	baseWait time.Duration
	// governor, when set, observes the rate-limit signal on every response so
	// the proactive pacing layer can spread the budget. Nil on the refresh
	// path (no pacing) — observation is skipped entirely, so there's no cost.
	governor *RateGovernor
	// reporter surfaces backoff sleeps as a "waiting" status so a 429/5xx
	// retry pause never looks like a hang. Defaults to a no-op.
	reporter progress.Reporter
}

func newBackoffClient() *backoffClient {
	return &backoffClient{
		inner: &http.Client{Timeout: 60 * time.Second},
		// 5 tries @ 1s base = waits of 1, 2, 4, 8, 16s on worst case; plenty
		// for transient backend blips and short-burst rate limits.
		maxTries: 5,
		baseWait: time.Second,
		reporter: progress.Nop(),
	}
}

// do executes req with retries. Caller owns the body (we buffer it so retries
// can re-send). Returns the final response + its body.
func (c *backoffClient) do(ctx context.Context, req *http.Request, body []byte) (*http.Response, []byte, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxTries; attempt++ {
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		}

		resp, err := c.inner.Do(req)
		if err != nil {
			lastErr = err
			if sleepErr := sleepOrCancel(ctx, c.baseWait*time.Duration(1<<attempt)); sleepErr != nil {
				return nil, nil, sleepErr
			}
			continue
		}
		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if sleepErr := sleepOrCancel(ctx, c.baseWait*time.Duration(1<<attempt)); sleepErr != nil {
				return nil, nil, sleepErr
			}
			continue
		}

		// Feed the proactive governor every response's rate signal.
		if c.governor != nil {
			c.governor.observeHeaders(resp.Header)
		}

		// Retryable: 429 (rate limit) or any 5xx.
		if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600) {
			wait := c.baseWait * time.Duration(1<<attempt)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
					wait = time.Duration(secs) * time.Second
				}
			}
			if c.governor != nil {
				c.governor.observeRetryAfter(wait)
			}
			lastErr = fmt.Errorf("%s %s → %d: %s", req.Method, req.URL.Path, resp.StatusCode, truncate(respBody, 200))
			if c.reporter != nil {
				reason := "backoff"
				if resp.StatusCode == http.StatusTooManyRequests {
					reason = "rate limit"
				}
				c.reporter.Wait(wait, reason)
			}
			if sleepErr := sleepOrCancel(ctx, wait); sleepErr != nil {
				return nil, nil, sleepErr
			}
			continue
		}

		return resp, respBody, nil
	}
	return nil, nil, fmt.Errorf("exhausted %d retries: %w", c.maxTries, lastErr)
}

// doJSON is the common case: send req, decode JSON into out on 2xx.
// out may be nil if the caller only needs the status check.
func (c *backoffClient) doJSON(ctx context.Context, req *http.Request, body []byte, out any) error {
	resp, respBody, err := c.do(ctx, req, body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s → %d: %s", req.Method, req.URL.Path, resp.StatusCode, truncate(respBody, 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Path, err)
	}
	return nil
}

func sleepOrCancel(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
