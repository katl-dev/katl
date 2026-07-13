package operatorconsole

import "time"

const HandoffPath = "/run/katl/console/handoff.json"

type Mode string

const (
	ModeInstaller Mode = "installer"
	ModeRuntime   Mode = "runtime"
)

type Snapshot struct {
	Mode                Mode
	Version             string
	Hostname            string
	State               string
	CurrentStep         string
	InputMode           string
	TargetDisk          string
	Generation          string
	GenerationBoot      string
	GenerationHealth    string
	DestructiveMutation bool
	LastError           string
	RetryHint           string
	Handoff             Handoff
	Network             []NetworkInterface
	SSHEnabled          bool
	UpdatedAt           time.Time
	StatusError         string
}

type NetworkInterface struct {
	Name      string
	Addresses []string
}

type Handoff struct {
	URL       string    `json:"url"`
	Token     string    `json:"token"`
	UpdatedAt time.Time `json:"updatedAt"`
}
