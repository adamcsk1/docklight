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
	Commit     string
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

// MasterClaim identifies an installation, not an authentication credential.
type MasterClaim struct {
	Coordinator string
	Master      string
	Name        string
}

type MaintenanceRequest struct {
	Operation string
	Claim     MasterClaim
	Run       string
	Confirmed bool
}

type MaintenanceState struct {
	NodeID string
	Owner  MasterClaim
	Run    string
	Lease  int64
}

type UpdateTarget struct {
	ID, Service, Role, Name, Image, Commit, State, Health, Error string
	Self                                                         bool
}

type MaintenanceInfo struct {
	Version        int
	HostID, Commit string
	State          MaintenanceState
	Busy           bool
	Targets        []UpdateTarget
}

const (
	ProjectLabel         = "com.docker.compose.project"
	ServiceLabel         = "com.docker.compose.service"
	WorkingDirLabel      = "com.docker.compose.project.working_dir"
	FilesLabel           = "com.docker.compose.project.config_files"
	EnvironmentFileLabel = "com.docker.compose.project.environment_file"
	RoleLabel            = "io.docklight.role"
)
