// Package version holds ofpack's version as a single compiled-in constant.
//
// It is a constant rather than a variable set with -ldflags because ofpack
// builds a second copy of itself: `pack --target` compiles the embedded
// source to produce the bin/ofpack that travels inside an archive. That
// build has no way to receive the linker flags the release build used, so a
// version injected at link time was present in the binary a user ran and
// absent from the one the archive carried. Everything that reports a
// version - the host binary, the manifest, and the bundled unpacker - reads
// this.
//
// release-please rewrites the line between the markers; do not edit it by
// hand.
package version

// x-release-please-start-version
const Version = "0.1.0"

// x-release-please-end
