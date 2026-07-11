package handoff

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	token              string
	validate           func([]byte) error
	defaultKatlosImage manifest.KatlosImage

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

func NewHandoffServer(token string, validate func([]byte) error) (*HandoffServer, error) {
	return NewHandoffServerWithDefaultImage(token, validate, manifest.KatlosImage{})
}

func NewHandoffServerWithDefaultImage(token string, validate func([]byte) error, defaultImage manifest.KatlosImage) (*HandoffServer, error) {
	if strings.TrimSpace(token) == "" {
		generated, err := GenerateHandoffToken()
		if err != nil {
			return nil, err
		}
		token = generated
	}
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
		token:              token,
		validate:           validate,
		defaultKatlosImage: defaultImage,
		state:              HandoffWaiting,
		status:             status,
	}, nil
}

func GenerateHandoffToken() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate handoff token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *HandoffServer) Token() string {
	return s.token
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
	defer s.mu.Unlock()

	return HandoffStatus{
		State:            s.state,
		ManifestAccepted: len(s.manifest) > 0,
		BundleAccepted:   len(s.bundle) > 0,
		SelectedNode:     s.nodeName,
		InstallStatus:    s.status,
	}
}

func (s *HandoffServer) Announcement(baseURL string) string {
	return fmt.Sprintf("katlos-install waiting for config at %s/v1/config-bundle token=%s", strings.TrimRight(baseURL, "/"), s.token)
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
	if !s.authorized(r) {
		http.Error(w, "missing or invalid token", http.StatusUnauthorized)
		return
	}

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
	if !s.authorized(r) {
		http.Error(w, "missing or invalid token", http.StatusUnauthorized)
		return
	}
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

func (s *HandoffServer) authorized(r *http.Request) bool {
	if r.Header.Get("X-Katl-Install-Token") == s.token {
		return true
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+s.token
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
