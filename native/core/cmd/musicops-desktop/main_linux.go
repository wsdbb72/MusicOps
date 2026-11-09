//go:build !windows && !darwin

// The desktop shell is Windows/macOS only: the Wails window needs a
// platform webview (WebView2 on Windows, WKWebView on macOS). On Linux the
// Go core serves the same WebUI directly.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "musicops-desktop: the desktop shell requires Windows or macOS")
	os.Exit(1)
}
