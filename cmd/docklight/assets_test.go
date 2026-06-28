package main

import (
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

func TestGeneratedHTMXMatchesPackageVersion(t *testing.T) {
	content, err := webFS.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatalf("expected generated HTMX asset; run npm run sync:htmx: %v", err)
	}
	match := regexp.MustCompile(`version:\s*"([^"]+)"`).FindSubmatch(content)
	if match == nil {
		t.Fatal("expected generated HTMX asset to include version")
	}

	packageContent, err := os.ReadFile("../../package.json")
	if err != nil {
		t.Fatal(err)
	}
	var packageJSON struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(packageContent, &packageJSON); err != nil {
		t.Fatal(err)
	}
	got, want := string(match[1]), packageJSON.Dependencies["htmx.org"]
	if len(want) > 0 && (want[0] == '^' || want[0] == '~') {
		want = want[1:]
	}
	if got != want {
		t.Fatalf("generated HTMX version %s does not match package.json %s", got, packageJSON.Dependencies["htmx.org"])
	}
}
