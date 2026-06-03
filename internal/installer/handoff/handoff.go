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

	"github.com/zariel/katl/internal/installer/manifest"
)

type HandoffState string

const (
	HandoffWaiting  HandoffState = "waiting-for-config"
	HandoffAccepted HandoffState = "install-starting"
)

type HandoffServer struct {
	token    string
	validate func([]byte) error

	mu       sync.Mutex
	state    HandoffState
	manifest []byte
}

type HandoffStatus struct {
	State            HandoffState `json:"state"`
	ManifestAccepted bool         `json:"manifestAccepted"`
}

func NewHandoffServer(token string, validate func([]byte) error) (*HandoffServer, error) {
	if strings.TrimSpace(token) == "" {
		generated, err := GenerateHandoffToken()
		if err != nil {
			return nil, err
		}
		token = generated
	}
	if validate == nil {
		validate = ValidateInstallManifestEnvelope
	}

	return &HandoffServer{
		token:    token,
		validate: validate,
		state:    HandoffWaiting,
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

func (s *HandoffServer) Status() HandoffStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	return HandoffStatus{
		State:            s.state,
		ManifestAccepted: len(s.manifest) > 0,
	}
}

func (s *HandoffServer) Announcement(baseURL string) string {
	return fmt.Sprintf("katlos-install waiting for config at %s/v1/install token=%s", strings.TrimRight(baseURL, "/"), s.token)
}

func (s *HandoffServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/install", s.handleInstall)
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

	s.mu.Lock()
	if s.state != HandoffWaiting {
		s.mu.Unlock()
		http.Error(w, "install already started", http.StatusConflict)
		return
	}

	s.manifest = append([]byte(nil), body...)
	s.state = HandoffAccepted
	status := HandoffStatus{
		State:            s.state,
		ManifestAccepted: true,
	}
	s.mu.Unlock()

	writeJSON(w, status)
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
