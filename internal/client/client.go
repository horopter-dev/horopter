// Package client reads Horopter's egress API: the changes a window and filters select, and
// one change with its events. It follows the next link egress returns until a result is
// complete, so its callers see rows, not pages.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client reads one egress with one key.
type Client struct {
	base *url.URL
	key  string
	http *http.Client
}

// New returns a Client for the egress at baseURL, authenticating with key, a reader key.
func New(baseURL, key string) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("no egress URL: set --egress-url or HOROPTER_EGRESS_URL")
	}
	if key == "" {
		return nil, errors.New("no key: set --key or HOROPTER_EGRESS_KEY to a reader key")
	}
	base, err := url.Parse(strings.TrimSuffix(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("egress URL %q: %w", baseURL, err)
	}
	return &Client{base: base, key: key, http: &http.Client{Timeout: time.Minute}}, nil
}

// ChangeQuery selects changes overlapping From..To. Limit caps the total across pages; zero
// reads them all.
type ChangeQuery struct {
	From, To                             time.Time
	Subject, Type, Status, Locus, Source string
	Attrs                                map[string]string
	Limit                                int
}

// Change is one change: the fields horo displays, and egress's own object in Raw.
type Change struct {
	Ref     string          `json:"ref"`
	Subject string          `json:"subject"`
	Type    string          `json:"type"`
	Title   string          `json:"title"`
	Actor   string          `json:"actor"`
	Status  string          `json:"status"`
	Start   time.Time       `json:"start"`
	End     *time.Time      `json:"end,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

// Event is one event: the fields horo displays, and egress's own object in Raw.
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Actor      string          `json:"actor"`
	Title      string          `json:"title"`
	OccurredAt time.Time       `json:"occurred_at"`
	Raw        json.RawMessage `json:"-"`
}

// HTTPError is an answer egress gave other than 200, with the message from its body.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("egress refused the key (401): %s; HOROPTER_EGRESS_KEY must be a reader key",
			e.Message)
	}
	return fmt.Sprintf("egress answered %d: %s", e.Status, e.Message)
}

// Changes yields every change q selects, following next until it is absent or q.Limit rows
// have been yielded.
func (c *Client) Changes(ctx context.Context, q ChangeQuery) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		n := 0
		// A change's start can move later while a caller pages — a started event arriving
		// after an annotation — carrying it past the cursor onto a later page as well. Each
		// ref is yielded once.
		seen := map[string]bool{}
		for row, err := range rows(ctx, c, "/v1/changes?"+changeParams(q).Encode(), "changes") {
			if err != nil {
				yield(Change{}, err)
				return
			}
			var ch Change
			if err := json.Unmarshal(row, &ch); err != nil {
				yield(Change{}, fmt.Errorf("decode a change: %w", err))
				return
			}
			ch.Raw = row
			if seen[ch.Ref] {
				continue
			}
			seen[ch.Ref] = true
			if !yield(ch, nil) {
				return
			}
			n++
			if q.Limit > 0 && n >= q.Limit {
				return
			}
		}
	}
}

// Change returns the change ref names, and whether there is one.
func (c *Client) Change(ctx context.Context, ref string) (Change, bool, error) {
	for row, err := range rows(ctx, c, "/v1/changes?"+url.Values{"ref": {ref}}.Encode(), "changes") {
		if err != nil {
			return Change{}, false, err
		}
		var ch Change
		if err := json.Unmarshal(row, &ch); err != nil {
			return Change{}, false, fmt.Errorf("decode a change: %w", err)
		}
		ch.Raw = row
		return ch, true, nil
	}
	return Change{}, false, nil
}

// Events yields every event of the change ref names, in id order.
func (c *Client) Events(ctx context.Context, ref string) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		for row, err := range rows(ctx, c, "/v1/events?"+url.Values{"ref": {ref}}.Encode(), "events") {
			if err != nil {
				yield(Event{}, err)
				return
			}
			var ev Event
			if err := json.Unmarshal(row, &ev); err != nil {
				yield(Event{}, fmt.Errorf("decode an event: %w", err))
				return
			}
			ev.Raw = row
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func changeParams(q ChangeQuery) url.Values {
	v := url.Values{}
	v.Set("from", q.From.UTC().Format(time.RFC3339Nano))
	v.Set("to", q.To.UTC().Format(time.RFC3339Nano))
	for name, value := range map[string]string{
		"subject": q.Subject, "type": q.Type, "status": q.Status,
		"locus": q.Locus, "source": q.Source,
	} {
		if value != "" {
			v.Set(name, value)
		}
	}
	for k, value := range q.Attrs {
		v.Set("attr."+k, value)
	}
	return v
}

// rows yields the raw rows of field from path and every page next links to.
func rows(ctx context.Context, c *Client, path, field string) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		for path != "" {
			page, next, err := c.page(ctx, path, field)
			if err != nil {
				yield(nil, err)
				return
			}
			if len(page) == 0 && next != "" {
				yield(nil, fmt.Errorf("egress returned a page with no rows and a next link (%s); "+
					"refusing to request it again", next))
				return
			}
			for _, row := range page {
				if !yield(row, nil) {
					return
				}
			}
			path = next
		}
	}
}

func (c *Client) page(ctx context.Context, path, field string) ([]json.RawMessage, string, error) {
	resp, err := c.fetch(ctx, path)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodePage(resp, field)
}

// fetch sends one authenticated GET for path, resolved under the client's base URL, and turns
// any answer but 200 into an HTTPError carrying egress's message.
func (c *Client) fetch(ctx context.Context, path string) (*http.Response, error) {
	ref, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("next link %q: %w", path, err)
	}
	u := *c.base
	u.Path = c.base.Path + ref.Path
	u.RawQuery = ref.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", u.Redacted(), err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Error == "" {
		body.Error = strconv.Itoa(resp.StatusCode) + " " + http.StatusText(resp.StatusCode)
	}
	return nil, &HTTPError{Status: resp.StatusCode, Message: body.Error}
}

// decodePage reads an envelope's rows under field and its next link, "" on the last page.
func decodePage(resp *http.Response, field string) ([]json.RawMessage, string, error) {
	where := resp.Request.URL.Redacted()
	var body map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", where, err)
	}
	var page []json.RawMessage
	if err := json.Unmarshal(body[field], &page); err != nil {
		return nil, "", fmt.Errorf("decode %s: %q: %w", where, field, err)
	}
	var next string
	if raw, ok := body["next"]; ok {
		if err := json.Unmarshal(raw, &next); err != nil {
			return nil, "", fmt.Errorf("decode next: %w", err)
		}
	}
	return page, next, nil
}
