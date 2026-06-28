package main

import (
	"net/http"
	"runtime"
	"strings"
)

type StackForm struct {
	Name                       string
	ComposeYAML                string
	ManagedFolders             []ManagedFolderForm
	ManagedFiles               []ManagedFileForm
	ExternalBindMountsAllowed  bool
	DockerSocketMountsAllowed  bool
	USBDeviceBindMountsAllowed bool
	DBusBindMountAllowed       bool
	HostPIDAllowed             bool
	PrivilegedAllowed          bool
	SecurityOptAllowed         bool
}

type ManagedFolderForm struct {
	Path  string
	Chmod string
	Chown string
}

type ManagedFileForm struct {
	Path    string
	Content string
	Chmod   string
	Chown   string
}

func newStackFormFromRequest(r *http.Request) StackForm {
	return StackForm{
		Name:                       strings.TrimSpace(r.FormValue("name")),
		ComposeYAML:                strings.TrimSpace(r.FormValue("compose_yaml")),
		ManagedFolders:             managedFoldersFromRequest(r),
		ManagedFiles:               managedFilesFromRequest(r),
		ExternalBindMountsAllowed:  r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed:  r.FormValue("docker_socket_mounts_allowed") == "on",
		USBDeviceBindMountsAllowed: r.FormValue("usb_device_bind_mounts_allowed") == "on",
		DBusBindMountAllowed:       r.FormValue("dbus_bind_mount_allowed") == "on",
		HostPIDAllowed:             r.FormValue("host_pid_allowed") == "on",
		PrivilegedAllowed:          r.FormValue("privileged_allowed") == "on",
		SecurityOptAllowed:         r.FormValue("security_opt_allowed") == "on",
	}
}

func updateStackFormFromRequest(r *http.Request, name string) StackForm {
	return StackForm{
		Name:                       name,
		ComposeYAML:                strings.TrimSpace(r.FormValue("compose_yaml")),
		ManagedFolders:             managedFoldersFromRequest(r),
		ManagedFiles:               managedFilesFromRequest(r),
		ExternalBindMountsAllowed:  r.FormValue("external_bind_mounts_allowed") == "on",
		DockerSocketMountsAllowed:  r.FormValue("docker_socket_mounts_allowed") == "on",
		USBDeviceBindMountsAllowed: r.FormValue("usb_device_bind_mounts_allowed") == "on",
		DBusBindMountAllowed:       r.FormValue("dbus_bind_mount_allowed") == "on",
		HostPIDAllowed:             r.FormValue("host_pid_allowed") == "on",
		PrivilegedAllowed:          r.FormValue("privileged_allowed") == "on",
		SecurityOptAllowed:         r.FormValue("security_opt_allowed") == "on",
	}
}

func managedFoldersFromRequest(r *http.Request) []ManagedFolderForm {
	paths := r.Form["managed_folder_path"]
	chmods := r.Form["managed_folder_chmod"]
	chowns := r.Form["managed_folder_chown"]
	folders := make([]ManagedFolderForm, 0, len(paths))
	for i, rawPath := range paths {
		folder := ManagedFolderForm{Path: strings.TrimSpace(rawPath)}
		if i < len(chmods) {
			folder.Chmod = strings.TrimSpace(chmods[i])
		}
		if i < len(chowns) {
			folder.Chown = strings.TrimSpace(chowns[i])
		}
		if folder.Path == "" && folder.Chmod == "" && folder.Chown == "" {
			continue
		}
		folders = append(folders, folder)
	}
	return folders
}

func parseManagedFolderForms(forms []ManagedFolderForm) ([]StackManagedConfigFolder, error) {
	folders := make([]StackManagedConfigFolder, 0, len(forms))
	for _, form := range forms {
		folders = append(folders, StackManagedConfigFolder{Path: form.Path, Chmod: form.Chmod, Chown: form.Chown})
	}
	parsed, err := parseManagedFolders(folders)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		for _, folder := range parsed {
			if folder.Chown != "" {
				return nil, errWindowsChownUnsupported("bind folder", folder.Path)
			}
		}
	}
	return parsed, nil
}

func managedFilesFromRequest(r *http.Request) []ManagedFileForm {
	paths := r.Form["managed_file_path"]
	contents := r.Form["managed_file_content"]
	chmods := r.Form["managed_file_chmod"]
	chowns := r.Form["managed_file_chown"]
	files := make([]ManagedFileForm, 0, len(paths))
	for i, rawPath := range paths {
		file := ManagedFileForm{Path: strings.TrimSpace(rawPath)}
		if i < len(contents) {
			file.Content = contents[i]
		}
		if i < len(chmods) {
			file.Chmod = strings.TrimSpace(chmods[i])
		}
		if i < len(chowns) {
			file.Chown = strings.TrimSpace(chowns[i])
		}
		if file.Path == "" && strings.TrimSpace(file.Content) == "" && file.Chmod == "" && file.Chown == "" {
			continue
		}
		files = append(files, file)
	}
	return files
}

func validateManagedFileFormPlatform(files []managedFileWrite) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	for _, file := range files {
		if file.Chown != "" {
			return errWindowsChownUnsupported("managed file", file.Path)
		}
	}
	return nil
}
