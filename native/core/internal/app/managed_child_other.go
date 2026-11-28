//go:build !windows

package app

import "os"

// Unix process groups are already stopped explicitly by App.Close. The
// Windows crash-safe job object has no equivalent here, so this is a no-op.
func prepareManagedChild() error { return nil }

func attachManagedChild(_ *os.Process) error { return nil }
