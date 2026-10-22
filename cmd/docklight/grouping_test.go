package main

import (
	"reflect"
	"testing"

	"docklight/internal/management"
)

func TestGroupContainers(t *testing.T) {
	input := []management.Container{
		{ID: "standalone", Name: "solo", State: "running"},
		{ID: "z", Name: "z-last", Project: "media", State: "exited"},
		{ID: "monitor", Name: "grafana", Project: "monitoring", State: "paused"},
		{ID: "a", Name: "a-first", Project: "media", State: "running", Service: "jellyfin", Protected: true, ComposeAvailable: true},
		{ID: "named", Name: "named", Project: "Standalone containers", State: "running"},
		{ID: "a-replica", Name: "a-first", Project: "media", State: "running", Service: "jellyfin"},
	}
	before := append([]management.Container(nil), input...)
	got := groupContainers(input)
	if !reflect.DeepEqual(before, input) {
		t.Fatal("grouping modified the input inventory")
	}
	if len(got) != 4 {
		t.Fatalf("groups: %#v", got)
	}
	for i, project := range []string{"Standalone containers", "media", "monitoring", ""} {
		if got[i].Project != project {
			t.Fatalf("wrong project order: %#v", got)
		}
	}
	media := got[1]
	if media.Running != 2 || len(media.Containers) != 3 {
		t.Fatalf("wrong running/total counts: %#v", media)
	}
	for i, id := range []string{"a", "a-replica", "z"} {
		if media.Containers[i].ID != id {
			t.Fatalf("wrong container order: %#v", media.Containers)
		}
	}
	if !media.Containers[0].Protected || !media.Containers[0].ComposeAvailable || media.Containers[0].Service != "jellyfin" {
		t.Fatal("grouping lost container action metadata")
	}
	if got[2].Running != 0 || got[3].Running != 1 {
		t.Fatal("paused/standalone counts incorrect")
	}
	for _, empty := range [][]management.Container{nil, {}} {
		if len(groupContainers(empty)) != 0 {
			t.Fatal("empty inventory produced phantom groups")
		}
	}
}
