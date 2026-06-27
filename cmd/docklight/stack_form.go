package main

import (
	"net/http"
	"strings"
)

type StackForm struct {
	Name                       string
	ComposeYAML                string
	BindFolders                string
	ManagedFiles               []ManagedFileForm
	ExternalBindMountsAllowed  bool
	DockerSocketMountsAllowed  bool
	USBDeviceBindMountsAllowed bool
	HostPIDAllowed             bool
}

type ManagedFileForm struct {
	Path    string
	Content string
}

func newStackFormFromRequest(r *http.Request) StackForm {
	return StackForm{
		Name:                       strings.TrimSpace(r.FormValue("name")),
		ComposeYAML:                strings.TrimSpace(r.FormValue("compose_yaml")),
		BindFolders:                strings.TrimSpace(r.FormValue("bind_folders")),
		ManagedFiles:               managedFilesFromRequest(r),
		ExternalBindMountsAllowed:  r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed:  r.FormValue("docker_socket_mounts_allowed") == "on",
		USBDeviceBindMountsAllowed: r.FormValue("usb_device_bind_mounts_allowed") == "on",
		HostPIDAllowed:             r.FormValue("host_pid_allowed") == "on",
	}
}

func updateStackFormFromRequest(r *http.Request, name string) StackForm {
	return StackForm{
		Name:                       name,
		ComposeYAML:                strings.TrimSpace(r.FormValue("compose_yaml")),
		BindFolders:                strings.TrimSpace(r.FormValue("bind_folders")),
		ManagedFiles:               managedFilesFromRequest(r),
		ExternalBindMountsAllowed:  r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed:  r.FormValue("docker_socket_mounts_allowed") == "on",
		USBDeviceBindMountsAllowed: r.FormValue("usb_device_bind_mounts_allowed") == "on",
		HostPIDAllowed:             r.FormValue("host_pid_allowed") == "on",
	}
}

func managedFilesFromRequest(r *http.Request) []ManagedFileForm {
	paths := r.Form["managed_file_path"]
	contents := r.Form["managed_file_content"]
	files := make([]ManagedFileForm, 0, len(paths))
	for i, rawPath := range paths {
		file := ManagedFileForm{Path: strings.TrimSpace(rawPath)}
		if i < len(contents) {
			file.Content = contents[i]
		}
		if file.Path == "" && strings.TrimSpace(file.Content) == "" {
			continue
		}
		files = append(files, file)
	}
	return files
}
