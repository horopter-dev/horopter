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

// Refold re-folds the stored <locus>/<ref> change from its log, retrying as Post does.
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
