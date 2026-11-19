package ghostlink

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink/delivery"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink/fingerprint"
)

const ContainerRoot = "/etc/x-ui/ghostlink"

var ErrInstallRequired = errors.New("activate GhostLink before configuring an inbound")
var wireEncoding = base64.RawURLEncoding.Strict()
var installPaths = defaultInstallPaths()

type Installer struct {
	root, binary, license string
	collect               func() (fingerprint.Bundle, error)
	client                *http.Client
	mu                    sync.Mutex
}

type InstallStatus struct {
	Ready       bool                `json:"ready"`
	Configured  bool                `json:"configured"`
	State       string              `json:"status"`
	RequestID   string              `json:"request_id,omitempty"`
	Fingerprint *fingerprint.Bundle `json:"fingerprint,omitempty"`
}

type installRequest struct {
	ID, Token, Hub, MachineID, State string
	Private                          []byte
}

func defaultInstallPaths() *Installer {
	if InContainer() {
		return newInstaller(ContainerRoot, filepath.Join(ContainerRoot, "ghostlinkd"), filepath.Join(ContainerRoot, "license.lic"))
	}
	return newInstaller(filepath.Dir(DefaultGhostLinkdConfig), DefaultGhostLinkdBinary,
		filepath.Join(filepath.Dir(DefaultGhostLinkdConfig), "license.lic"))
}

func newInstaller(root, binary, license string) *Installer {
	return &Installer{root: root, binary: binary, license: license, collect: fingerprint.Collect,
		client: &http.Client{Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

func InContainer() bool { return os.Getenv("XUI_IN_DOCKER") == "true" }

func DefaultInstaller() *Installer { return installPaths }

func RequireInstallation() error {
	if (InContainer() || os.Getenv("GHOSTLINK_LICENSE_HUB_URL") != "") && !installPaths.Ready() {
		return ErrInstallRequired
	}
	return nil
}

func hubEndpoint() (string, error) {
	raw := strings.TrimRight(strings.TrimSpace(os.Getenv("GHOSTLINK_LICENSE_HUB_URL")), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("configure GHOSTLINK_LICENSE_HUB_URL with an HTTPS endpoint")
	}
	return raw, nil
}

func (i *Installer) Ready() bool {
	if pending, err := i.loadStatus(); err != nil || pending.State == "verified" {
		return false
	}
	return i.installedFilesReady()
}

func (i *Installer) installedFilesReady() bool {
	exe, err := os.Lstat(i.binary)
	if err != nil || !exe.Mode().IsRegular() || (runtime.GOOS != "windows" && exe.Mode().Perm()&0111 == 0) {
		return false
	}
	info, err := os.Lstat(i.license)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) || info.Size() > 64<<10 {
		return false
	}
	data, err := os.ReadFile(i.license)
	return err == nil && licenseIsCurrent(data)
}

// Readiness previews expiry only. The daemon enforces the signed claims.
func licenseIsCurrent(data []byte) bool {
	var env struct {
		Payload string `json:"payload_b64"`
	}
	if len(data) > 64<<10 || json.Unmarshal(data, &env) != nil {
		return false
	}
	payload, err := wireEncoding.DecodeString(env.Payload)
	var claims struct {
		ExpiresAt int64 `json:"expires_at"`
	}
	return err == nil && json.Unmarshal(payload, &claims) == nil && claims.ExpiresAt >= time.Now().Unix()
}

func (i *Installer) Status() (InstallStatus, error) {
	endpoint, endpointErr := hubEndpoint()
	pending, err := i.loadStatus()
	if err != nil {
		return InstallStatus{}, err
	}
	result := InstallStatus{
		Ready:      pending.State != "verified" && i.installedFilesReady(),
		Configured: endpointErr == nil && runtime.GOOS == "linux",
		State:      pending.State, RequestID: pending.ID,
	}
	// Existing host installations continue to use their systemd configuration.
	if !InContainer() && os.Getenv("GHOSTLINK_LICENSE_HUB_URL") == "" {
		exe, err := os.Stat(i.binary)
		result.Ready = err == nil && exe.Mode().IsRegular() && exe.Mode().Perm()&0111 != 0
	}
	if result.Ready {
		result.State = "installed"
		return result, nil
	}
	if pending.Hub != "" && pending.Hub != endpoint && pending.active() && pending.State != "verified" {
		result.State = "hub_changed"
	}
	if result.State == "" || result.State == "installed" {
		result.State = "inactive"
	}
	if fp, err := i.collect(); err == nil {
		preview := fp.Preview()
		result.Fingerprint = &preview
	}
	return result, nil
}

func (p installRequest) active() bool {
	return p.State == "pending" || p.State == "downloading" || p.State == "verified"
}

func (i *Installer) Start(ctx context.Context, note string) error {
	if !i.mu.TryLock() {
		return errors.New("GhostLink installation is already in progress")
	}
	defer i.mu.Unlock()
	base, err := hubEndpoint()
	if err != nil {
		return err
	}
	if len(note) > 1000 {
		return errors.New("request note exceeds 1000 bytes")
	}
	pending, err := i.loadStatus()
	if err != nil {
		return err
	}
	if pending.State == "verified" || (pending.active() && pending.Hub == base) {
		return errors.New("GhostLink installation is already in progress")
	}
	if i.installedFilesReady() {
		return errors.New("GhostLink is already installed")
	}
	fp, err := i.collect()
	if err != nil {
		return err
	}
	if err := fp.Validate(); err != nil {
		return err
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return errors.New("unsupported GhostLink core architecture")
	}
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	requestBody, err := json.Marshal(struct {
		Fingerprint fingerprint.Bundle `json:"fingerprint"`
		Public      string             `json:"client_eph_pub"`
		Arch        string             `json:"arch"`
		Note        string             `json:"note"`
	}{fp, delivery.PublicString(key.PublicKey()), "linux-" + runtime.GOARCH, note})
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(i.root); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/requests", bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("License Hub returned HTTP %d", resp.StatusCode)
	}
	var status delivery.Status
	if err := decodeStatus(resp.Body, &status); err != nil || status.State != "pending" ||
		!validID(status.ID) || len(status.Token) != 52 {
		return errors.New("invalid License Hub application response")
	}
	private := key.Bytes()
	defer clear(private)
	return i.save(installRequest{ID: status.ID, Token: status.Token, Hub: base, MachineID: fp.MachineID, State: "pending", Private: private})
}

func (i *Installer) Step(ctx context.Context) error {
	if !i.mu.TryLock() {
		return nil
	}
	defer i.mu.Unlock()
	p, err := i.load()
	if err != nil || !p.active() {
		return err
	}
	defer clear(p.Private)
	if p.State == "verified" {
		return i.finish(p)
	}
	base, err := hubEndpoint()
	if err != nil || p.Hub != base {
		// Never send a pending request credential to a newly configured hub.
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	resp, err := i.get(ctx, p, "/status")
	if err != nil {
		return err
	}
	var status delivery.Status
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return i.complete(p, "failed")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("License Hub status returned HTTP %d", resp.StatusCode)
	}
	err = decodeStatus(resp.Body, &status)
	resp.Body.Close()
	if err != nil || status.ID != p.ID {
		return errors.New("invalid License Hub status response")
	}
	switch status.State {
	case "pending":
		return nil
	case "rejected", "expired", "consumed":
		return i.complete(p, status.State)
	case "approved":
		p.State = "downloading"
		if err := i.save(p); err != nil {
			return err
		}
		return i.download(ctx, p)
	default:
		return errors.New("unknown License Hub request status")
	}
}

func decodeStatus(r io.Reader, dst *delivery.Status) error {
	return json.NewDecoder(io.LimitReader(r, 4096)).Decode(dst)
}

func (i *Installer) get(ctx context.Context, p installRequest, suffix string) (*http.Response, error) {
	if !validID(p.ID) {
		return nil, errors.New("invalid pending request ID")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.Hub+"/api/v1/requests/"+p.ID+suffix, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	return i.client.Do(req)
}

func validID(id string) bool {
	return len(id) == 26 && strings.IndexFunc(id, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '2' || r > '7')
	}) == -1
}

func (i *Installer) download(ctx context.Context, p installRequest) error {
	key, err := ecdh.X25519().NewPrivateKey(p.Private)
	if err != nil {
		return errors.Join(err, i.complete(p, "failed"))
	}
	if err := os.MkdirAll(filepath.Dir(i.binary), 0755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(i.binary), ".ghostlinkd-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	resp, err := i.get(ctx, p, "/bundle")
	if err != nil {
		return err // Status on the next poll determines whether it was consumed.
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return i.complete(p, "consumed")
	}
	if resp.StatusCode == http.StatusConflict {
		return i.complete(p, "failed")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("License Hub bundle returned HTTP %d", resp.StatusCode)
	}
	contentType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if contentType != delivery.ContentType {
		return errors.Join(errors.New("unknown bundle content type"), i.complete(p, "failed"))
	}
	meta, err := delivery.Receive(temp, resp.Body, key, p.ID, p.MachineID)
	if err != nil || !licenseIsCurrent(meta.License) {
		return errors.Join(errors.New("GhostLink bundle verification failed"), err, i.complete(p, "failed"))
	}
	if err := temp.Chmod(0755); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), i.binary+".new"); err != nil {
		return err
	}
	if err := syncInstallDir(i.binary); err != nil {
		return err
	}
	if err := writeInstallFile(i.license+".new", meta.License); err != nil {
		return err
	}
	clear(p.Private)
	p.Private, p.Token, p.MachineID, p.Hub = nil, "", "", ""
	p.State = "verified"
	if err := i.save(p); err != nil {
		return err
	}
	return i.finish(p)
}

func (i *Installer) finish(p installRequest) error {
	// The verified journal disables startup until both staged files commit.
	// A panel restart safely resumes either rename.
	for _, path := range []string{i.binary, i.license} {
		if err := os.Rename(path+".new", path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncInstallDir(path); err != nil {
			return err
		}
	}
	if !i.installedFilesReady() {
		return i.complete(p, "failed")
	}
	return i.complete(p, "installed")
}

func (i *Installer) complete(p installRequest, state string) error {
	clear(p.Private)
	return i.save(installRequest{ID: p.ID, State: state})
}

func (i *Installer) loadStatus() (installRequest, error) {
	var status struct{ ID, Hub, State string }
	err := i.readJournal(&status)
	return installRequest{ID: status.ID, Hub: status.Hub, State: status.State}, err
}

func (i *Installer) load() (installRequest, error) {
	var p installRequest
	err := i.readJournal(&p)
	return p, err
}

func (i *Installer) readJournal(dst any) error {
	file, err := os.Open(filepath.Join(i.root, "activation.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewDecoder(io.LimitReader(file, 8192)).Decode(dst)
}

func (i *Installer) save(p installRequest) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	defer clear(data)
	return writeInstallFile(filepath.Join(i.root, "activation.json"), data)
}

func writeInstallFile(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".activation-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	return syncInstallDir(path)
}

func syncInstallDir(path string) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
