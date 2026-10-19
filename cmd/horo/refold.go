package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"horopter.dev/horopter/internal/client"
)

const adminHint = "--key / HOROPTER_ADMIN_KEY must be a key with the admin capability " +
	"(tenant_admin or superuser)"

// refNotFound opens the only 404 that exits 3; a mux or unknown-tenant 404 is an error.
const refNotFound = "no events for ref"

// runRefold re-folds one change, with a key variable of its own so no other key is sent.
func runRefold(
	ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer,
) int {
	fs := flag.NewFlagSet("refold", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("ingress-url", env("HOROPTER_INGRESS_URL"), "ingress base URL")
	key := env("HOROPTER_ADMIN_KEY")
	fs.Var(secret{&key}, "key", "a key with the admin capability (default $HOROPTER_ADMIN_KEY)")
	tenant := fs.String("tenant", "", "the tenant to act in, for a global superuser key")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		say(stderr, "horo refold: give exactly one ref, as horo show prints it, e.g. ci/dep-1")
		return exitUsage
	}
	if err := refoldTarget(*url, key); err != nil {
		say(stderr, "horo refold:", err)
		return exitUsage
	}
	cl, err := client.New(*url, key)
	if err != nil {
		say(stderr, "horo refold:", err)
		return exitUsage
	}
	got, err := cl.Refold(ctx, fs.Arg(0), *tenant, time.Now().Add(endWindow))
	if err != nil {
		return refoldFailed(stderr, err)
	}
	if err := json.NewEncoder(stdout).Encode(got); err != nil {
		return report(stderr, "horo refold", fmt.Errorf("write the answer: %w", err))
	}
	return exitOK
}

func refoldTarget(url, key string) error {
	switch {
	case url == "":
		return errors.New("no ingress URL: set --ingress-url or HOROPTER_INGRESS_URL")
	case key == "":
		return errors.New("no key: set --key or HOROPTER_ADMIN_KEY to a key with the " +
			"admin capability")
	}
	return nil
}

func refoldFailed(stderr io.Writer, err error) int {
	var he *client.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound &&
		strings.HasPrefix(he.Message, refNotFound) {
		sayf(stderr, "horo refold: %v\n", err)
		return exitNotFound
	}
	if errors.As(err, &he) &&
		(he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden) {
		return report(stderr, "horo refold", fmt.Errorf("%w; %s", err, adminHint))
	}
	return report(stderr, "horo refold", err)
}
