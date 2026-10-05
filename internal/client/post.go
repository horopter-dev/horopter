package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// attemptTimeout caps one request, so a server that never answers costs at most this much of
// the caller's deadline before the next attempt.
const attemptTimeout = 5 * time.Second

// NewEvent is one event to append. It never carries a source: ingress takes that from the key.
type NewEvent struct {
	Type, Subject, Title, Actor, CorrelationRef, IdempotencyKey, Lifecycle string
	OccurredAt                                                             time.Time
	Attributes                                                             map[string]any
}

// Appended is ingress's answer to an append.
type Appended struct {
	ID             string `json:"id"`
	Ref            string `json:"ref"`
	CorrelationRef string `json:"correlation_ref"`
	Created        bool   `json:"created"`
}

// Post appends ev, retrying network errors and 503 with backoff until retryUntil. retryUntil
// bounds the whole call, every attempt included.
func (c *Client) Post(ctx context.Context, ev NewEvent, retryUntil time.Time) (Appended, error) {
	ctx, cancel := context.WithDeadline(ctx, retryUntil)
	defer cancel()
	body, err := json.Marshal(eventBody(ev))
	if err != nil {
		return Appended{}, fmt.Errorf("encode event: %w", err)
	}
	backoff := 200 * time.Millisecond
	for {
		got, err := c.postOnce(ctx, body)
		if err == nil || !retryable(err) {
			return got, err
		}
		//nolint:gosec // G404: backoff jitter spreads retries; it needs no unpredictability.
		wait := backoff/2 + rand.N(backoff)
		select {
		case <-ctx.Done():
			return Appended{}, fmt.Errorf("gave up at the deadline: %w", err)
		case <-time.After(wait):
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

func eventBody(ev NewEvent) map[string]any {
	b := map[string]any{
		"type": ev.Type, "subject": ev.Subject,
		"occurred_at": ev.OccurredAt.UTC().Format(time.RFC3339Nano),
	}
	for k, v := range map[string]string{
		"title": ev.Title, "actor": ev.Actor, "correlation_ref": ev.CorrelationRef,
		"idempotency_key": ev.IdempotencyKey, "lifecycle": ev.Lifecycle,
	} {
		if v != "" {
			b[k] = v
		}
	}
	if len(ev.Attributes) > 0 {
		b["attributes"] = ev.Attributes
	}
	return b
}

// retryable reports an error a later attempt can fix: no answer at all, or a 503.
func retryable(err error) bool {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == http.StatusServiceUnavailable
	}
	return true
}

func (c *Client) postOnce(ctx context.Context, body []byte) (Appended, error) {
	ctx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()
	u := *c.base
	u.Path = c.base.Path + "/v1/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Appended{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Appended{}, fmt.Errorf("post %s: %w", u.Redacted(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
		}
		return Appended{}, &HTTPError{Status: resp.StatusCode, Message: e.Error}
	}
	var got Appended
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		return Appended{}, fmt.Errorf("decode ingress's answer: %w", err)
	}
	return got, nil
}
