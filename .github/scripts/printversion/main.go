// Command printversion writes ofpack's compiled-in version, so CI can
// compare it against the binary and the release manifest without parsing
// Go source.
package main

import (
	"fmt"

	"github.com/konono/ofpack/internal/version"
)

func main() { fmt.Println(version.Version) }
