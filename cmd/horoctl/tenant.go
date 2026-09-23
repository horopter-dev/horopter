package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
)

func runAdminTenant(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: horoctl admin tenant <create|list>")
	}
	switch args[0] {
	case "create":
		return runTenantCreate(args[1:])
	case "list":
		return runTenantList(args[1:])
	default:
		return fmt.Errorf("unknown admin tenant command %q", args[0])
	}
}

// runTenantCreate mints an opaque tenant id and prints it alone on stdout, mirroring keys
// issue's stdout/stderr split: the id is what a caller pipes into --tenant elsewhere, and
// it is opaque precisely so nothing downstream can mistake the display name for it.
func runTenantCreate(args []string) error {
	fs := flag.NewFlagSet("admin tenant create", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	displayName := fs.String("display-name", "", "human-readable, mutable tenant name")
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}
	if *displayName == "" {
		return errors.New("--display-name is required")
	}

	id, err := newTenantID()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		`INSERT INTO tenant (id, display_name) VALUES ($1, $2)`, id, *displayName); err != nil {
		return fmt.Errorf("create tenant %q: %w", *displayName, err)
	}

	fmt.Fprintf(os.Stderr, "created tenant %s (display name %q)\n", id, *displayName)
	fmt.Println(id)
	return nil
}

func runTenantList(args []string) error {
	fs := flag.NewFlagSet("admin tenant list", flag.ContinueOnError)
	dsn := dsnFlag(fs)
	if err := fs.Parse(args); err != nil {
		return helpOrErr(err)
	}

	ctx := context.Background()
	pool, err := openPool(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `SELECT id, display_name FROM tenant ORDER BY created_at`)
	if err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		fmt.Printf("%s\t%s\n", id, name)
	}
	return rows.Err()
}
