package core

import "fmt"

// Origin distinguishes human and process event sources.
type Origin string

// Values for Origin.
const (
	OriginHuman   Origin = "human"
	OriginProcess Origin = "process"
)

// OriginForCredentialType maps credential types to their event source.
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
