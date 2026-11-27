package management

import (
	"context"
	"encoding/json"
	"strings"
)

// ImageSnapshot describes deployed images, not the potentially newer local tags.
// It contains no container environment, mounts or registry credentials.
type ImageSnapshot struct {
	Version          int
	NodeID, Hostname string
	Containers       []ImageContainer
}

type ImageContainer struct {
	ID, Name, Image, ImageID, Project, Service string
	OS, Architecture, Variant                  string
	Skip, Error                                string
}

func (e *Engine) ImageSnapshot(ctx context.Context) (ImageSnapshot, error) {
	snapshot := ImageSnapshot{Version: 1}
	containers, err := e.containers(ctx)
	if err != nil {
		return snapshot, err
	}
	hostname, err := e.capture(ctx, "info", "--format", "{{.Name}}")
	if err != nil {
		return snapshot, err
	}
	snapshot.Hostname = strings.TrimSpace(hostname)
	type image struct {
		ID, OS, Architecture, Variant string
		RepoDigests                   []string
	}
	images := map[string]image{}
	for _, c := range containers {
		v := ImageContainer{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, ImageID: c.Image, Project: c.Config.Labels[ProjectLabel], Service: c.Config.Labels[ServiceLabel]}
		switch {
		case c.Config.Labels[RoleLabel] == "updater":
			v.Skip = "Temporary update helper"
		case c.Config.Labels["io.docklight.updates"] == "false":
			v.Skip = "Disabled by container label"
		case strings.Contains(c.Config.Image, "@"):
			v.Skip = "Digest-pinned image"
		case strings.HasPrefix(c.Config.Image, "sha256:"):
			v.Skip = "Image ID without registry tag"
		default:
			i, ok := images[c.Image]
			if !ok {
				text, captureErr := e.capture(ctx, "image", "inspect", c.Image)
				var inspected []image
				if captureErr != nil || json.Unmarshal([]byte(text), &inspected) != nil || len(inspected) != 1 || inspected[0].ID != c.Image {
					v.Error = "Deployed image metadata unavailable"
				} else {
					i = inspected[0]
					images[c.Image] = i
				}
			}
			v.OS, v.Architecture, v.Variant = i.OS, i.Architecture, i.Variant
			if v.Error == "" && len(i.RepoDigests) == 0 {
				v.Skip = "Local image without registry provenance"
			}
		}
		snapshot.Containers = append(snapshot.Containers, v)
	}
	return snapshot, nil
}
