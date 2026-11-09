//go:build windows

package desktop

import "os"

// statUID has no meaning on Windows; private_directory skips the owner
// check there, mirroring platforms.py.
func statUID(info os.FileInfo) (int, bool) { return 0, false }
