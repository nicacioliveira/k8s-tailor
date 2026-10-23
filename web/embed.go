// Package webui embeds the built React bundle so the Go binary serves the UI.
//
// dist/ always contains the versioned placeholder .gitkeep, so a clean clone
// builds (`go build ./...`) without the UI; the server then answers 503 "UI not
// built" on / while the API works. `make ui-build` / the Dockerfile drop the real
// dist/index.html in before building.
package webui

import "embed"

//go:embed dist/*
var Files embed.FS
