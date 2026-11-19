package ghostlink

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink/delivery"
	"github.com/mhsanaei/3x-ui/v3/internal/ghostlink/fingerprint"
)

func TestInstallerStagesVerifiedBundleAndErasesCredentials(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("A", 26)
	token := strings.Repeat("Z", 52)
	fp := fingerprint.Bundle{MachineID: strings.Repeat("1", 64), MACs: []string{strings.Repeat("2", 64)}}
	plain := bytes.Repeat([]byte{11, 22, 33}, delivery.ChunkSize)
	digest := sha256.Sum256(plain)
	payload, err := json.Marshal(map[string]int64{"expires_at": time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	license, err := json.Marshal(map[string]string{"payload_b64": base64.RawURLEncoding.EncodeToString(payload)})
	if err != nil {
		t.Fatal(err)
	}
	var calls, approved atomic.Int32
	var submitted atomic.Pointer[delivery.Sender]
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/requests", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Public      string             `json:"client_eph_pub"`
			Fingerprint fingerprint.Bundle `json:"fingerprint"`
			Note        string             `json:"note"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !body.Fingerprint.Matches(fp) || body.Note != "local test" {
			t.Errorf("bad submission: %+v %v", body, err)
			w.WriteHeader(400)
			return
		}
		clientKey, err := delivery.ParsePublic(body.Public)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		sender, err := delivery.NewSender(clientKey, id, fp.MachineID, hex.EncodeToString(digest[:]), license)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		submitted.Store(sender)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(delivery.Status{ID: id, State: "pending", Token: token})
	})
	mux.HandleFunc("GET /api/v1/requests/"+id+"/status", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(404)
			return
		}
		state := "pending"
		if approved.Load() == 1 {
			state = "approved"
		}
		_ = json.NewEncoder(w).Encode(delivery.Status{ID: id, State: state})
	})
	mux.HandleFunc("GET /api/v1/requests/"+id+"/bundle", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token || approved.Load() != 1 {
			w.WriteHeader(403)
			return
		}
		w.Header().Set("Content-Type", delivery.ContentType)
		if err := submitted.Load().Write(w, bytes.NewReader(plain)); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	t.Setenv("GHOSTLINK_LICENSE_HUB_URL", server.URL)
	t.Setenv("XUI_IN_DOCKER", "true")
	i := newInstaller(root, filepath.Join(root, "ghostlinkd"), filepath.Join(root, "license.lic"))
	i.collect = func() (fingerprint.Bundle, error) { return fp, nil }
	i.client = server.Client()

	if err := i.Step(t.Context()); err != nil || calls.Load() != 0 {
		t.Fatalf("idle installer talked to Hub: %v", err)
	}
	view, err := i.Status()
	if err != nil || view.Fingerprint == nil || view.Fingerprint.MachineID == fp.MachineID || view.Ready {
		t.Fatalf("inactive status: %+v, %v", view, err)
	}
	if err := i.Start(t.Context(), "local test"); err != nil {
		t.Fatal(err)
	}
	// A new panel process can resume using the protected pending journal.
	reopened := newInstaller(root, i.binary, i.license)
	reopened.client = server.Client()
	if err := reopened.Step(t.Context()); err != nil || reopened.Ready() {
		t.Fatalf("pending installer step: %v", err)
	}
	approved.Store(1)
	if err := reopened.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(i.binary)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("core mismatch: %v", err)
	}
	got, err = os.ReadFile(i.license)
	if err != nil || !bytes.Equal(got, license) {
		t.Fatalf("license mismatch: %v", err)
	}
	if runtime.GOOS == "linux" {
		for path, want := range map[string]os.FileMode{i.binary: 0755, i.license: 0600, filepath.Join(root, "activation.json"): 0600} {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("%s permissions: %v %v", path, info, err)
			}
		}
		if !reopened.Ready() {
			t.Fatal("verified installation is not ready")
		}
	}
	finished, err := reopened.load()
	if err != nil || finished.State != "installed" || len(finished.Private) != 0 || finished.Token != "" {
		t.Fatalf("activation retained credentials or wrong state: %v %s", err, finished.State)
	}
	before := calls.Load()
	if err := reopened.Step(t.Context()); err != nil || calls.Load() != before {
		t.Fatalf("completed install contacted Hub: %v", err)
	}
}
