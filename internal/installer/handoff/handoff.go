package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/katl-dev/katl/internal/installer/configbundle"
	"github.com/katl-dev/katl/internal/installer/discovery"
	"github.com/katl-dev/katl/internal/installer/disk"
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
	extensionLayout    fs.FS

	mu                                 sync.Mutex
	state                              HandoffState
	manifest                           []byte
	bundle                             []byte
	nodeName                           string
	destructiveStorageAcknowledgements []string
	status                             installstatus.Record
	discovery                          discovery.DiscoverySource
	sshAccess                          SSHAccessStatus
	configureSSH                       func(context.Context, []string) error
}

type HandoffStatus struct {
	Image            manifest.KatlosImage `json:"image,omitzero"`
	State            HandoffState         `json:"state"`
	ManifestAccepted bool                 `json:"manifestAccepted"`
	BundleAccepted   bool                 `json:"bundleAccepted,omitempty"`
	SelectedNode     string               `json:"selectedNode,omitempty"`
	InstallStatus    installstatus.Record `json:"installStatus"`
	Disks            []HandoffDisk        `json:"disks,omitempty"`
	SSHAccess        SSHAccessStatus      `json:"sshAccess,omitzero"`
}

type SSHAccessStatus struct {
	Enabled            bool   `json:"enabled"`
	Account            string `json:"account,omitempty"`
	AuthorizedKeyCount int    `json:"authorizedKeyCount,omitempty"`
}

type SSHAccessRequest struct {
	AuthorizedKeys []string `json:"authorizedKeys"`
}

// HandoffDisk reports a whole disk and the identities that can select it.
type HandoffDisk struct {
	Path       string   `json:"path"`
	ByID       []string `json:"byID,omitempty"`
	WWN        string   `json:"wwn,omitempty"`
	Serial     string   `json:"serial,omitempty"`
	ByPath     []string `json:"byPath,omitempty"`
	GPTGUID    string   `json:"gptGUID,omitempty"`
	Model      string   `json:"model,omitempty"`
	Type       string   `json:"type"`
	SizeBytes  uint64   `json:"sizeBytes"`
	ReadOnly   bool     `json:"readOnly,omitempty"`
	Mounted    bool     `json:"mounted,omitempty"`
	Selectable bool     `json:"selectable"`
}

type BundlePayload struct {
	Data                               []byte
	NodeName                           string
	DestructiveStorageAcknowledgements []string
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

func (s *HandoffServer) DestructiveStorageAcknowledgements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.destructiveStorageAcknowledgements...)
}

func (s *HandoffServer) Bundle() BundlePayload {
	s.mu.Lock()
	defer s.mu.Unlock()

	return BundlePayload{
		Data:                               append([]byte(nil), s.bundle...),
		NodeName:                           s.nodeName,
		DestructiveStorageAcknowledgements: append([]string(nil), s.destructiveStorageAcknowledgements...),
	}
}

func (s *HandoffServer) Status() HandoffStatus {
	return s.statusAt(context.Background())
}

func (s *HandoffServer) statusAt(ctx context.Context) HandoffStatus {
	disks := s.inventory(ctx)
	s.mu.Lock()
	status := HandoffStatus{
		Image:            s.defaultKatlosImage,
		State:            s.state,
		ManifestAccepted: len(s.manifest) > 0,
		BundleAccepted:   len(s.bundle) > 0,
		SelectedNode:     s.nodeName,
		InstallStatus:    s.status,
		Disks:            disks,
		SSHAccess:        s.sshAccess,
	}
	reader := s.statusReader
	s.mu.Unlock()

	if reader != nil {
		if durable, err := reader(); err == nil {
			if !durable.UpdatedAt.Before(status.InstallStatus.UpdatedAt) {
				status.InstallStatus = durable
			}
		}
	}
	return status
}

// BeginAutomatic reserves the installer before exposing the API so a caller
// cannot submit a competing installation while boot-provided inputs run.
func (s *HandoffServer) BeginAutomatic(nodeName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = HandoffAccepted
	s.nodeName = nodeName
	s.status = installstatus.New(installstatus.StateRunning, time.Now().UTC())
	s.status.InputMode = installstatus.InputModePXEPreseed
}

// PrepareRetry returns the handoff server to its waiting state after an
// installer attempt failed without mutating the target disk. The failure
// remains visible through Status until another request is accepted.
func (s *HandoffServer) PrepareRetry(status installstatus.Record) bool {
	if status.DestructiveMutation {
		return false
	}
	switch status.State {
	case installstatus.StateFailedBeforeMutation, installstatus.StateInstallRefused:
	default:
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != HandoffAccepted {
		return false
	}
	s.state = HandoffWaiting
	s.manifest = nil
	s.bundle = nil
	s.nodeName = ""
	s.destructiveStorageAcknowledgements = nil
	s.status = status
	return true
}

// SetDiscovery sets the source of the disk inventory. Each status and
// validation request rediscovers, so disks attached or removed while the
// installer waits are reflected without a reboot.
func (s *HandoffServer) SetDiscovery(source discovery.DiscoverySource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discovery = source
}

// discover returns the current hardware facts, or none when the installer
// has no discovery source.
func (s *HandoffServer) discover(ctx context.Context) (discovery.HardwareFacts, error) {
	s.mu.Lock()
	source := s.discovery
	s.mu.Unlock()
	if source == nil {
		return discovery.HardwareFacts{}, nil
	}
	return source.Discover(ctx)
}

// inventory reports the whole disks present now. A failed discovery reports
// none, as for an installer that could not discover hardware.
func (s *HandoffServer) inventory(ctx context.Context) []HandoffDisk {
	facts, err := s.discover(ctx)
	if err != nil {
		return nil
	}
	return handoffDisks(facts)
}

func handoffDisks(facts discovery.HardwareFacts) []HandoffDisk {
	disks := make([]HandoffDisk, 0, len(facts.BlockDevices))
	for _, device := range facts.BlockDevices {
		if device.Type != discovery.DeviceDisk {
			continue
		}
		mounted := len(device.Mountpoints) > 0
		for _, partition := range device.Partitions {
			mounted = mounted || len(partition.Mountpoints) > 0
		}
		for _, mount := range facts.Mounts {
			if mount.Source == device.Path {
				mounted = true
			}
			for _, partition := range device.Partitions {
				if mount.Source == partition.Path {
					mounted = true
				}
			}
		}
		byID := append([]string(nil), device.ByID...)
		sort.Strings(byID)
		byPath := append([]string(nil), device.ByPath...)
		sort.Strings(byPath)
		gptGUID := ""
		if device.PartitionSignature == "gpt" {
			gptGUID = device.PartitionTableUUID
		}
		// Every writable, unmounted whole disk can be selected: by name and
		// size if it reports no stronger identity.
		disks = append(disks, HandoffDisk{
			Path: device.Path, ByID: byID, WWN: device.WWN, Serial: device.Serial,
			ByPath: byPath, GPTGUID: gptGUID, Model: device.Model, Type: discovery.DiskType(device),
			SizeBytes: device.SizeBytes, ReadOnly: device.ReadOnly,
			Mounted: mounted, Selectable: !device.ReadOnly && !mounted,
		})
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].Path < disks[j].Path })
	return disks
}

func (s *HandoffServer) SetStatusReader(reader func() (installstatus.Record, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusReader = reader
}

func (s *HandoffServer) SetSSHConfigurer(configure func(context.Context, []string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configureSSH = configure
}

func (s *HandoffServer) Announcement(baseURL string) string {
	return "katlos-install waiting for config at " + strings.TrimRight(baseURL, "/") + "/v1/config-bundle"
}

func (s *HandoffServer) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.extensionLayout != nil {
		mux.Handle("GET /v1/extension-layout/", http.StripPrefix("/v1/extension-layout/", http.FileServerFS(s.extensionLayout)))
	}
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/install", s.handleInstall)
	mux.HandleFunc("POST /v1/config-bundle", s.handleConfigBundle)
	mux.HandleFunc("POST /v1/ssh-access", s.handleSSHAccess)
	return mux
}

func (s *HandoffServer) handleSSHAccess(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		http.Error(w, "read SSH access request", http.StatusBadRequest)
		return
	}
	var request SSHAccessRequest
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid SSH access request: "+err.Error(), http.StatusBadRequest)
		return
	}
	keys := request.AuthorizedKeys
	if len(keys) == 0 {
		http.Error(w, "selected node has no SSH authorized keys", http.StatusBadRequest)
		return
	}

	for _, key := range keys {
		if strings.ContainsAny(key, "\r\n\x00") || !manifest.ValidAuthorizedKey(key) {
			http.Error(w, "SSH access requires valid public keys", http.StatusBadRequest)
			return
		}
	}
	disks := s.inventory(r.Context())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != HandoffWaiting {
		http.Error(w, "installer SSH can only be configured while waiting for config", http.StatusConflict)
		return
	}
	if s.configureSSH == nil {
		http.Error(w, "installer SSH configuration is unavailable", http.StatusNotImplemented)
		return
	}
	if err := s.configureSSH(r.Context(), keys); err != nil {
		http.Error(w, "configure installer SSH: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.sshAccess = SSHAccessStatus{
		Enabled:            true,
		Account:            "root",
		AuthorizedKeyCount: len(keys),
	}
	writeJSON(w, HandoffStatus{
		State:         s.state,
		InstallStatus: s.status,
		Disks:         disks,
		SSHAccess:     s.sshAccess,
	})
}

func (s *HandoffServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *HandoffServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.statusAt(r.Context()))
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
	acknowledgements, err := destructiveStorageAcknowledgements(r)
	if err != nil {
		http.Error(w, "invalid destructive storage acknowledgement: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validateStorage(r.Context(), decoded); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	s.destructiveStorageAcknowledgements = acknowledgements
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
	acknowledgements, err := destructiveStorageAcknowledgements(r)
	if err != nil {
		http.Error(w, "invalid destructive storage acknowledgement: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validateStorage(r.Context(), selected.InstallManifest); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	s.destructiveStorageAcknowledgements = acknowledgements
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

func destructiveStorageAcknowledgements(r *http.Request) ([]string, error) {
	acknowledgements := append([]string(nil), r.URL.Query()["acknowledgeStorageWipe"]...)
	for i := range acknowledgements {
		acknowledgements[i] = strings.TrimSpace(acknowledgements[i])
	}
	sort.Strings(acknowledgements)
	if err := disk.ValidateDestructiveVolumeAcknowledgementKeys(acknowledgements); err != nil {
		return nil, err
	}
	return acknowledgements, nil
}

// validateStorage checks storage selectors against the disks present now.
// Planning rediscovers and revalidates after acceptance, so a disk that
// changes in between is still caught before any mutation.
func (s *HandoffServer) validateStorage(ctx context.Context, installManifest manifest.Manifest) error {
	facts, err := s.discover(ctx)
	if err != nil {
		return fmt.Errorf("plan storage targets: discover disks: %w", err)
	}
	if len(facts.BlockDevices) == 0 {
		return nil
	}
	root, err := discovery.MatchTargetDisk(facts, installManifest.Install.TargetDisk.Target())
	if err != nil {
		return fmt.Errorf("plan storage targets: resolve install root disk: %w", err)
	}
	_, err = disk.PlanVolumes(facts, root.Device, manifest.BuildVolumeRequests(installManifest.Install.Volumes), nil)
	if err != nil {
		return fmt.Errorf("plan storage targets: %w", err)
	}
	return nil
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
