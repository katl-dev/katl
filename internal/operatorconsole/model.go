package operatorconsole

import "time"

const HandoffPath = "/run/katl/console/handoff.json"

type Mode string

const (
	ModeInstaller Mode = "installer"
	ModeRuntime   Mode = "runtime"
)

type Snapshot struct {
	Mode                 Mode
	Version              string
	Hostname             string
	State                string
	CurrentStep          string
	Generation           string
	GenerationHealth     string
	CurrentSoftware      Software
	NextBootSoftware     Software
	LiveSoftware         Software
	KubernetesConfigured bool
	DestructiveMutation  bool
	LastError            string
	RetryHint            string
	Handoff              Handoff
	ManagementAddress    string
	DisplayInterfaces    []NetworkInterface
	AdditionalInterfaces int
	SSHEnabled           bool
	UpdatedAt            time.Time
	StatusStale          bool
	StatusError          string
	HandoffError         string
	GenerationError      string
}

type Software struct {
	Generation        string
	KatlOSVersion     string
	KubernetesVersion string
}

type PresentationState string

const (
	PresentationHealthy     PresentationState = "healthy"
	PresentationProgressing PresentationState = "progressing"
	PresentationDegraded    PresentationState = "degraded"
	PresentationFailed      PresentationState = "failed"
	PresentationUnknown     PresentationState = "unknown"
)

type Presentation struct {
	State PresentationState
	Label string
}

type DashboardModel struct {
	Host       Presentation
	Kubernetes Presentation
	Current    Software
	NextBoot   Software
	Live       Software
}

type NetworkInterface struct {
	Name                string
	Addresses           []string
	AdditionalAddresses int
}

type Handoff struct {
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
}
