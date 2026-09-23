package core

import "fmt"

// Origin distinguishes human and process event sources.
type Origin string

// Values for Origin.
const (
	OriginHuman   Origin = "human"
	OriginProcess Origin = "process"
)

// Validate reports whether o is one of the two values the log stores.
func (o Origin) Validate() error {
	switch o {
	case OriginHuman, OriginProcess:
		return nil
	default:
		return fmt.Errorf("origin %q is not %q or %q", o, OriginHuman, OriginProcess)
	}
}

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
