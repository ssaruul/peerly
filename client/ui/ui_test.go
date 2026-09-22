//go:build !windows

package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"peerly/client/core"
	"peerly/proto"
	"peerly/server/api"
	"peerly/server/blobs"
	"peerly/server/store"
)

type fixture struct {
	app          *App
	local        *httptest.Server
	worldID      string
	savePath     string
	downloadSlow atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	blobDir, err := blobs.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	inner := (&api.Server{Store: database, Blobs: blobDir, MaxUpload: 1 << 30}).Handler()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/blob") {
			time.Sleep(time.Duration(f.downloadSlow.Load()))
		}
		inner.ServeHTTP(w, r)
	}))
	config, err := core.LoadConfig(filepath.Join(t.TempDir(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session, err := core.NewAPIClient(remote.URL, "").CreateGroup(ctx, "friends", "a")
	if err != nil {
		t.Fatal(err)
	}
	config.Update(func(stored *core.Config) {
		stored.ServerURL, stored.Token, stored.Member, stored.Group = remote.URL, session.Token, session.Member, session.Group
	})
	world, err := core.NewAPIClient(remote.URL, session.Token).CreateWorld(ctx, proto.CreateWorldRequest{Name: "base", GameName: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	f.worldID = world.ID
	f.savePath = filepath.Join(t.TempDir(), "Game", "Saves")
	os.MkdirAll(f.savePath, 0o750)
	os.WriteFile(filepath.Join(f.savePath, "world.sav"), []byte("day1;"), 0o644)
	f.app = New(config)
	f.local = httptest.NewServer(f.app.Handler())
	t.Cleanup(func() {
		f.app.StopAndWait(5 * time.Second)
		f.local.Close()
		remote.Close()
		database.Close()
	})
	return f
}

func (f *fixture) call(t *testing.T, method string, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	request, _ := http.NewRequest(method, f.local.URL+path, bytes.NewReader(encoded))
	request.Header.Set("X-Peerly", "1")
	for key, value := range headers {
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decoded := map[string]any{}
	json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

func (f *fixture) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for f.app.HostingActive() {
		if time.Now().After(deadline) {
			t.Fatal("session never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestGuardRejectsForeignAndForgedRequests(t *testing.T) {
	f := newFixture(t)
	f.app.AccessKey = "secret-key"
	withKey := map[string]string{"X-Peerly-Key": "secret-key"}
	if status, _ := f.call(t, "GET", "/api/state", nil, withKey); status != http.StatusOK {
		t.Fatalf("legitimate request = %d", status)
	}
	for name, headers := range map[string]map[string]string{
		"no access key":            {},
		"wrong access key":         {"X-Peerly-Key": "guess"},
		"cross-site browser fetch": {"X-Peerly-Key": "secret-key", "Sec-Fetch-Site": "cross-site"},
	} {
		if status, _ := f.call(t, "GET", "/api/state", nil, headers); status == http.StatusOK {
			t.Errorf("%s was served", name)
		}
	}
	if status, _ := f.call(t, "POST", "/api/leave", nil, map[string]string{"X-Peerly-Key": "secret-key", "X-Peerly": ""}); status != http.StatusForbidden {
		t.Errorf("state-changing request without the custom header = %d", status)
	}
	rebinding, _ := http.NewRequest("GET", f.local.URL+"/api/state", nil)
	rebinding.Host = "evil.example.com"
	rebinding.Header.Set("X-Peerly-Key", "secret-key")
	response, err := http.DefaultClient.Do(rebinding)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Errorf("DNS rebinding style Host header = %d", response.StatusCode)
	}
	if status, _ := f.call(t, "GET", "/api/ping", nil, map[string]string{}); status != http.StatusOK {
		t.Errorf("ping must work without a key so a second launch can find the running app, got %d", status)
	}
}

func TestSettingsMustBeConfirmedAndSafe(t *testing.T) {
	f := newFixture(t)
	if status, _ := f.call(t, "POST", "/api/worlds/"+f.worldID+"/host", nil, nil); status != http.StatusAccepted {
		t.Fatalf("host = %d", status)
	}
	f.waitIdle(t)
	_, state := f.call(t, "GET", "/api/state", nil, nil)
	if message, _ := state["hosting"].(map[string]any)["error"].(string); !strings.Contains(message, "Settings") {
		t.Fatalf("hosting an unconfirmed world: %q", message)
	}
	home, _ := os.UserHomeDir()
	status, answer := f.call(t, "PUT", "/api/worlds/"+f.worldID+"/settings", map[string]any{"save_path": filepath.Join(home, "Documents"), "launch": "true"}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("documents folder as save folder = %d %v", status, answer)
	}
	status, answer = f.call(t, "PUT", "/api/worlds/"+f.worldID+"/settings", map[string]any{"save_path": f.savePath, "launch": "steam://rungameid/1"}, nil)
	if status != http.StatusUnprocessableEntity || !strings.Contains(answer["error"].(string), "process name") {
		t.Fatalf("launcher link without process = %d %v", status, answer)
	}
}

func TestCopyingASaveBlocksHostingUntilItIsDone(t *testing.T) {
	f := newFixture(t)
	settings := map[string]any{"save_path": f.savePath, "include": "world.*", "launch": "true"}
	if status, answer := f.call(t, "PUT", "/api/worlds/"+f.worldID+"/settings", settings, nil); status != http.StatusOK {
		t.Fatalf("settings = %d %v", status, answer)
	}
	f.call(t, "POST", "/api/worlds/"+f.worldID+"/host", nil, nil)
	f.waitIdle(t)
	request, _ := http.NewRequest("GET", f.local.URL+"/api/worlds/"+f.worldID+"/revisions", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	revisions := []proto.Revision{}
	json.NewDecoder(response.Body).Decode(&revisions)
	response.Body.Close()
	if len(revisions) == 0 {
		t.Fatal("no revision to copy")
	}

	f.downloadSlow.Store(int64(1500 * time.Millisecond))
	pulled := make(chan int, 1)
	go func() {
		status, _ := f.call(t, "POST", "/api/worlds/"+f.worldID+"/pull", map[string]string{"revision_id": revisions[0].ID}, nil)
		pulled <- status
	}()
	time.Sleep(400 * time.Millisecond)
	if status, answer := f.call(t, "POST", "/api/worlds/"+f.worldID+"/host", nil, nil); status != http.StatusConflict {
		t.Fatalf("host during a copy = %d %v", status, answer)
	}
	if status, _ := f.call(t, "POST", "/api/worlds/"+f.worldID+"/pull", map[string]string{"revision_id": revisions[0].ID}, nil); status != http.StatusConflict {
		t.Fatalf("second copy during a copy = %d", status)
	}
	if status := <-pulled; status != http.StatusOK {
		t.Fatalf("copy = %d", status)
	}
	if status, _ := f.call(t, "POST", "/api/worlds/"+f.worldID+"/host", nil, nil); status != http.StatusAccepted {
		t.Fatalf("host after the copy finished = %d", status)
	}
	f.waitIdle(t)
}

func TestShutdownReasonFollowsTheSession(t *testing.T) {
	f := newFixture(t)
	var mutex sync.Mutex
	reasons := []string{}
	f.app.OnBlockReason = func(reason string) {
		mutex.Lock()
		defer mutex.Unlock()
		reasons = append(reasons, reason)
	}
	settings := map[string]any{"save_path": f.savePath, "include": "world.*", "launch": "true"}
	if status, answer := f.call(t, "PUT", "/api/worlds/"+f.worldID+"/settings", settings, nil); status != http.StatusOK {
		t.Fatalf("settings = %d %v", status, answer)
	}
	f.call(t, "POST", "/api/worlds/"+f.worldID+"/host", nil, nil)
	f.waitIdle(t)
	mutex.Lock()
	defer mutex.Unlock()
	if len(reasons) < 2 || reasons[len(reasons)-1] != "" {
		t.Fatalf("reasons = %q", reasons)
	}
	sawUpload := false
	for _, reason := range reasons {
		if strings.Contains(reason, "uploading") {
			sawUpload = true
		}
	}
	if !sawUpload {
		t.Fatalf("no upload reason announced: %q", reasons)
	}
}

func TestChangingTheServerAddressKeepsMembershipAndRefusesStrangers(t *testing.T) {
	f := newFixture(t)
	before := f.app.config.Snapshot().ServerURL
	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			writeJSON(w, http.StatusOK, map[string]string{"service": "peerly"})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown token"})
	}))
	defer stranger.Close()
	if status, _ := f.call(t, "POST", "/api/server-url", map[string]string{"server_url": stranger.URL}, nil); status != http.StatusUnauthorized {
		t.Fatalf("switching to a server that does not know this PC = %d", status)
	}
	if status, _ := f.call(t, "POST", "/api/server-url", map[string]string{"server_url": "http://127.0.0.1:1"}, nil); status != http.StatusBadGateway {
		t.Fatalf("switching to a dead address = %d", status)
	}
	if f.app.config.Snapshot().ServerURL != before {
		t.Fatal("a refused switch changed the stored address")
	}
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, before+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	defer mirror.Close()
	status, answer := f.call(t, "POST", "/api/server-url", map[string]string{"server_url": mirror.URL}, nil)
	if status != http.StatusOK || answer["server_url"] != before {
		t.Fatalf("switching to a redirecting mirror of the same server = %d %v", status, answer)
	}
}
