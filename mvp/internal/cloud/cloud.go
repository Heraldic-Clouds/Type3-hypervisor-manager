package cloud

import "context"

type Instance struct{ ID, IP, DesktopID, State string }

// Compute is provider-independent for a future bare-metal adapter.
type Compute interface {
	Launch(context.Context, string) (Instance, error)
	Find(context.Context, string) ([]Instance, error)
	Terminate(context.Context, string) error
	Password(context.Context, string) (string, error)
	DeleteCredential(context.Context, string) error
	Ready(context.Context, string) (bool, error)
}
