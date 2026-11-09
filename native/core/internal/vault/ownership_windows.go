//go:build windows

package vault

import "os"

// checkDirOwnership is a no-op on Windows: directory ownership is enforced
// by ACLs, not by uid.
func checkDirOwnership(dir string, info os.FileInfo) error { return nil }
