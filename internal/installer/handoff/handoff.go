package handoff

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/manifest"
	installstatus "github.com/katl-dev/katl/internal/installer/status"
)

type HandoffState string

const (
	HandoffWaiting  HandoffState = "waiting-for-config"
	HandoffAccepted HandoffState = "install-starting"
)

type HandoffServer struct {
	validate           func([]byte) error
	defaultKatlosImage manifest.KatlosImage
	statusReader       func() (installstatus.Record, error)

	mu       sync.Mutex
	state    HandoffState
	manifest []byte
	bundle   []byte
	nodeName string
	status   installstatus.Record
}

type HandoffStatus struct {
	State            HandoffState         `json:"state"`
	ManifestAccepted bool                 `json:"manifestAccepted"`
	BundleAccepted   bool                 `json:"bundleAccepted,omitempty"`
	SelectedNode     string               `json:"selectedNode,omitempty"`
	InstallStatus    installstatus.Record `json:"installStatus"`
}

type BundlePayload struct {
	Data     []byte
	NodeName string
}

func NewHandoffServer(validate func([]byte) error) *HandoffServer {
	return NewHandoffServerWithDefaultImage(validate, manifest.KatlosImage{})
}

func NewHandoffServerWithDefaultImage(validate func([]byte) error, defaultImage manifest.KatlosImage) *HandoffServer {
	if validate == nil {
		validate = func(data []byte) error {
			_, _, err := manifest.DecodeWithDefaultImage(bytes.NewReader(data), defaultImage)
			return err
		}
	}

	status := installstatus.New(installstatus.StateWaitingForConfig, time.Now().UTC())
	status.InputMode = installstatus.InputModeLocalHandoff
	status.InputSource = installstatus.InputModeLocalHandoff
	return &HandoffServer{
		validate:           validate,
		defaultKatlosImage: defaultImage,
		state:              HandoffWaiting,
		status:             status,
	}
}

func (s *HandoffServer) Manifest() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]byte(nil), s.manifest...)
}

func (s *HandoffServer) Bundle() BundlePayload {
	s.mu.Lock()
	defer s.mu.Unlock()

	return BundlePayload{
		Data:     append([]byte(nil), s.bundle...),
		NodeName: s.nodeName,
	}
}

func (s *HandoffServer) Status() HandoffStatus {
	s.mu.Lock()
	status := HandoffStatus{
		State:            s.state,
		ManifestAccepted: len(s.manifest) > 0,
		BundleAccepted:   len(s.bundle) > 0,
		SelectedNode:     s.nodeName,
		InstallStatus:    s.status,
	}
	reader := s.statusReader
	s.mu.Unlock()

	if reader != nil {
		if durable, err := reader(); err == nil {
			status.InstallStatus = durable
		}
	}
	return status
}

func (s *HandoffServer) SetStatusReader(reader func() (installstatus.Record, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusReader = reader
}

func (s *HandoffServer) Announcement(baseURL string) string {
	return "katlos-install waiting for config at " + strings.TrimRight(baseURL, "/") + "/v1/config-bundle"
}

func (s *HandoffServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/install", s.handleInstall)
	mux.HandleFunc("POST /v1/config-bundle", s.handleConfigBundle)
	return mux
}

func (s *HandoffServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *HandoffServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Status())
}

func (s *HandoffServer) handleInstall(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read manifest", http.StatusBadRequest)
		return
	}
	if err := s.validate(body); err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	decoded, _, err := manifest.DecodeWithDefaultImage(bytes.NewReader(body), s.defaultKatlosImage)
	if err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}
	digest, err := installstatus.DigestManifest(decoded)
	if err != nil {
		http.Error(w, "invalid manifest: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.state != HandoffWaiting {
		s.mu.Unlock()
		http.Error(w, "install already started", http.StatusConflict)
		return
	}

	s.manifest = append([]byte(nil), body...)
	s.state = HandoffAccepted
	status := installstatus.New(installstatus.StateRunning, time.Now().UTC())
	status.InputMode = installstatus.InputModeLocalHandoff
	status.InputSource = installstatus.InputModeLocalHandoff
	status.RequestDigest = digest
	status.KatlosImage = installstatus.ImageFromManifest(decoded)
	status.CurrentStep = "WaitForLocalConfig"
	status.CompletedSteps = []string{"WaitForLocalConfig"}
	s.status = status
	response := HandoffStatus{
		State:            s.state,
		ManifestAccepted: true,
		InstallStatus:    s.status,
	}
	s.mu.Unlock()

	writeJSON(w, response)
}

func (s *HandoffServer) handleConfigBundle(w http.ResponseWriter, r *http.Request) {
	nodeName := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("node"), r.Header.Get("X-Katl-Node-Name")))
	expectedDigest := strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("digest"), r.Header.Get("X-Katl-Bundle-Digest")))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		http.Error(w, "read config bundle", http.StatusBadRequest)
		return
	}
	selected, err := configbundle.ReadSelectedNode(bytes.NewReader(body), configbundle.ReadOptions{
		ExpectedDigest:     expectedDigest,
		NodeName:           nodeName,
		DefaultKatlosImage: s.defaultKatlosImage,
	})
	if err != nil {
		http.Error(w, "invalid config bundle: "+err.Error(), http.StatusBadRequest)
		return
	}
	digest, err := installstatus.DigestManifest(selected.InstallManifest)
	if err != nil {
		http.Error(w, "invalid config bundle: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	if s.state != HandoffWaiting {
		s.mu.Unlock()
		http.Error(w, "install already started", http.StatusConflict)
		return
	}

	s.bundle = append([]byte(nil), body...)
	s.nodeName = selected.Node.Name
	s.state = HandoffAccepted
	status := installstatus.New(installstatus.StateRunning, time.Now().UTC())
	status.InputMode = installstatus.InputModeLocalHandoff
	status.InputSource = installstatus.InputModeLocalHandoff
	status.RequestDigest = digest
	status.BundleDigest = selected.BundleDigest
	status.SourceDigest = selected.SourceDigest
	status.NodeMaterialDigest = selected.NodeMaterialDigest
	status.InstallMaterialDigest = selected.InstallMaterialDigest
	status.KatlosImage = installstatus.ImageFromManifest(selected.InstallManifest)
	status.CurrentStep = "WaitForLocalConfig"
	status.CompletedSteps = []string{"WaitForLocalConfig"}
	s.status = status
	response := HandoffStatus{
		State:            s.state,
		ManifestAccepted: false,
		BundleAccepted:   true,
		SelectedNode:     s.nodeName,
		InstallStatus:    s.status,
	}
	s.mu.Unlock()

	writeJSON(w, response)
}

func ValidateInstallManifestEnvelope(data []byte) error {
	_, err := manifest.Decode(bytes.NewReader(data))
	return err
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, "encode response", http.StatusInternalServerError)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
