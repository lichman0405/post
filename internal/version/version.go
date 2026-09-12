// Package version records the POST build version, shared by all four
// cmd/ binaries. It is overridden at link time with
//
//	-ldflags "-X github.com/lichman0405/post/internal/version.Version=vX.Y.Z"
package version

// Version is the build version. It defaults to the development value and is
// stamped at release build time.
var Version = "0.1.0-dev"
