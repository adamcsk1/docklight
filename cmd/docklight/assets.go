package main

import "embed"

//go:embed templates static
var webFS embed.FS

//go:embed static/htmx.min.js
var htmxAsset []byte
