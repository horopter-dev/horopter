package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Refolded is ingress's answer to a re-fold.
type Refolded struct {
	Ref     string   `json:"ref"`
	Events  int      `json:"events"`
	Altered []string `json:"altered"`
}

// Refold re-folds one change from its log. ref is the stored <locus>/<ref> form; tenant is
// for a global key, and is sent only when set. A re-fold is idempotent, so it retries as Post
// does, until retryUntil.
func (c *Client) Refold(
	ctx context.Context, ref, tenant string, retryUntil time.Time,
) (Refolded, error) {
	req := map[string]string{"ref": ref}
	if tenant != "" {
		req["tenant"] = tenant
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Refolded{}, fmt.Errorf("encode re-fold request: %w", err)
	}
	var got Refolded
	err = c.postRetrying(ctx, "/v1/admin/refold", body, &got, retryUntil)
	return got, err
}
