package core

import "fmt"

// Origin indicates whether an event was written by a human or a process.
type Origin string

// Possible Origin values.
const (
	OriginHuman   Origin = "human"
	OriginProcess Origin = "process"
)

// OriginForCredentialType returns the Origin for a credential type.
func OriginForCredentialType(credentialType string) (Origin, error) {
	switch credentialType {
	case "machine_key":
		return OriginProcess, nil
	case "oidc":
		return OriginHuman, nil
	default:
		return Origin(""), fmt.Errorf("no origin for credential type %q", credentialType)
	}
}
