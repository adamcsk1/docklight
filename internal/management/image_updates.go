package management

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// ImageSnapshotVersion requires storage-aware deployed digests from the agent.
const ImageSnapshotVersion = 2

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
	ManifestDigest                             string
	Skip, Error                                string
}

// DeployedDigest is a platform manifest on containerd, or a config digest on the classic store.
func (c ImageContainer) DeployedDigest() string {
	if c.ManifestDigest != "" {
		return c.ManifestDigest
	}
	return c.ImageID
}

func (e *Engine) ImageSnapshot(ctx context.Context) (ImageSnapshot, error) {
	snapshot := ImageSnapshot{Version: ImageSnapshotVersion}
	containers, err := e.containers(ctx)
	if err != nil {
		return snapshot, err
	}
	metadata, err := e.capture(ctx, "info", "--format", `{"Name":{{json .Name}},"DriverStatus":{{json .DriverStatus}}}`)
	if err != nil {
		return snapshot, err
	}
	var info struct {
		Name         string
		DriverStatus [][2]string
	}
	if err := json.Unmarshal([]byte(metadata), &info); err != nil {
		return snapshot, errors.New("Docker image store metadata unavailable")
	}
	snapshot.Hostname = info.Name
	containerd := slices.ContainsFunc(info.DriverStatus, func(row [2]string) bool {
		return row[0] == "driver-type" && strings.HasPrefix(row[1], "io.containerd.snapshotter.")
	})
	type image struct {
		ID, OS, Architecture, Variant string
		RepoDigests                   []string
		Descriptor                    *imageDescriptor
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
			if v.Error == "" && v.Skip == "" && (containerd || i.Descriptor != nil || c.ImageManifestDescriptor != nil) {
				descriptor := c.ImageManifestDescriptor
				// A single manifest target is unambiguous even on older containerd APIs.
				if descriptor == nil && i.Descriptor != nil && isImageManifest(i.Descriptor.MediaType) {
					descriptor = i.Descriptor
				}
				if descriptor == nil || !isImageManifest(descriptor.MediaType) || descriptor.Digest == "" {
					v.Error = "Deployed platform manifest unavailable; update Docker"
				} else if descriptor.Platform != nil && descriptor.Platform.OS != "" && descriptor.Platform.Architecture != "" {
					v.ManifestDigest = descriptor.Digest
					v.OS, v.Architecture, v.Variant = descriptor.Platform.OS, descriptor.Platform.Architecture, descriptor.Platform.Variant
				} else if i.Descriptor != nil && isImageManifest(i.Descriptor.MediaType) && i.Descriptor.Digest == descriptor.Digest {
					v.ManifestDigest = descriptor.Digest
				} else {
					v.Error = "Deployed manifest platform unavailable; update Docker"
				}
			}
		}
		snapshot.Containers = append(snapshot.Containers, v)
	}
	return snapshot, nil
}

func isImageManifest(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.manifest.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}
