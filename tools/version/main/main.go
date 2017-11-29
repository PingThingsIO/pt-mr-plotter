package main

import (
	"fmt"
	"os"

	version "github.com/PingThingsIO/pt-mr-plotter/tools/version"
)

/**
/* While you could request a version by passing -version to the compiled binary for serveplotter.go
/* That won't work if you compile it for a different target OS (e.g. building for Linux on MacOS)
/* This creates a small binary that you can build for the host system
**/

func main() {
	fmt.Printf("%d.%d.%d\n", version.Major, version.Minor, version.Patch)
	os.Exit(0)
}
