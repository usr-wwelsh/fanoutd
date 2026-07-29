package main

import (
	"embed"
	"io/fs"
	"log"
)

// The built frontend ships inside the binary, so deploying is one file and the
// server does not care what directory it was started from.
//
//go:embed all:dist
var distFS embed.FS

func ui() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("embedded UI is unusable: %v", err)
	}
	return sub
}
