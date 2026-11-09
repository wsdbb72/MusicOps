// Package version carries the application version. The default is a
// development marker, never a release number: a real release injects its
// version at build time with
// -ldflags "-X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=x.y.z".
package version

// Version is the application version string.
var Version = "0.0.0-dev"
