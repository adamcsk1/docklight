// Package management implements Docklight's fixed, authenticated host API.
package management

type Container struct {
	ID               string
	Name             string
	Image            string
	ImageID          string
	State            string
	Project          string
	Service          string
	Protected        bool
	ComposeAvailable bool
	Buildable        bool
	BuildError       string
}

type Resource struct {
	ID        string
	Name      string
	Kind      string
	UsedBy    []string
	Protected bool
}

type Inventory struct {
	Containers []Container
	Resources  []Resource
}

type Action struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Scope     string `json:"scope"`
	Confirmed bool   `json:"confirmed"`
}

type Job struct {
	ID      string
	Action  Action
	Status  string
	Output  string
	Started string
}

const (
	ProjectLabel         = "com.docker.compose.project"
	ServiceLabel         = "com.docker.compose.service"
	WorkingDirLabel      = "com.docker.compose.project.working_dir"
	FilesLabel           = "com.docker.compose.project.config_files"
	EnvironmentFileLabel = "com.docker.compose.project.environment_file"
	RoleLabel            = "io.docklight.role"
)
