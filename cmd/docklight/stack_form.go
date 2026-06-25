package main

import (
	"net/http"
	"strings"
)

type StackForm struct {
	Name                      string
	ComposeYAML               string
	EnvFile                   string
	BindFolders               string
	RecreateFolders           bool
	ExternalBindMountsAllowed bool
	DockerSocketMountsAllowed bool
	HostPIDAllowed            bool
}

func newStackFormFromRequest(r *http.Request) StackForm {
	return StackForm{
		Name:                      strings.TrimSpace(r.FormValue("name")),
		ComposeYAML:               strings.TrimSpace(r.FormValue("compose_yaml")),
		EnvFile:                   strings.TrimRight(r.FormValue("env_file"), "\r\n"),
		BindFolders:               strings.TrimSpace(r.FormValue("bind_folders")),
		ExternalBindMountsAllowed: r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed: r.FormValue("docker_socket_mounts_allowed") == "on",
		HostPIDAllowed:            r.FormValue("host_pid_allowed") == "on",
	}
}

func updateStackFormFromRequest(r *http.Request, name string) StackForm {
	return StackForm{
		Name:                      name,
		ComposeYAML:               strings.TrimSpace(r.FormValue("compose_yaml")),
		EnvFile:                   strings.TrimRight(r.FormValue("env_file"), "\r\n"),
		BindFolders:               strings.TrimSpace(r.FormValue("bind_folders")),
		RecreateFolders:           r.FormValue("update_mode") != "save" && r.FormValue("recreate_folders") == "on",
		ExternalBindMountsAllowed: r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed: r.FormValue("docker_socket_mounts_allowed") == "on",
		HostPIDAllowed:            r.FormValue("host_pid_allowed") == "on",
	}
}
