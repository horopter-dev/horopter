package main

import (
	"errors"
	"fmt"
)

// runAdmin dispatches admin's sub-verbs: keys, tenant and bootstrap. None of them is ever
// delegated to PATH — admin itself is compiled in, and so is everything under it.
func runAdmin(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: horoctl admin <keys|tenant|bootstrap>")
	}
	switch args[0] {
	case "keys":
		return runAdminKeys(args[1:])
	case "tenant":
		return runAdminTenant(args[1:])
	case "bootstrap":
		return runAdminBootstrap(args[1:])
	default:
		return fmt.Errorf("unknown admin command %q", args[0])
	}
}
