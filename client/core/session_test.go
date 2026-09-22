//go:build !windows

package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"peerly/proto"
	"peerly/server/api"
	"peerly/server/blobs"
	"peerly/server/store"
)

type testServer struct {
	url         string
	store       *store.Store
	uploadDelay atomic.Int64
	onDownload  atomic.Pointer[func()]
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	blobDir, err := blobs.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	server := &testServer{store: database}
	inner := (&api.Server{Store: database, Blobs: blobDir, MaxUpload: 1 << 30}).Handler()
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/revisions") {
			time.Sleep(time.Duration(server.uploadDelay.Load()))
		}
		if hook := server.onDownload.Load(); hook != nil && strings.HasSuffix(r.URL.Path, "/blob") {
			(*hook)()
		}
		inner.ServeHTTP(w, r)
	}))
	server.url = httpServer.URL
	t.Cleanup(func() {
		httpServer.Close()
		database.Close()
	})
	return server
}

type player struct {
	name     string
	client   *APIClient
	config   *ConfigFile
	savePath string
	events   []Event
	mutex    sync.Mutex
}

func (p *player) session(worldID string) *Session {
	timings := DefaultTimings()
	timings.Heartbeat = 100 * time.Millisecond
	timings.Watch = 50 * time.Millisecond
	timings.Quiet = 150 * time.Millisecond
	timings.Settle = 100 * time.Millisecond
	timings.ProcessPoll = 50 * time.Millisecond
	timings.UploadRetry = 50 * time.Millisecond
	timings.Checkpoint = 200 * time.Millisecond
	return &Session{Client: p.client, Config: p.config, WorldID: worldID, Timings: timings, OnEvent: func(event Event) {
		p.mutex.Lock()
		defer p.mutex.Unlock()
		p.events = append(p.events, event)
	}}
}

func (p *player) sawEvent(kind EventKind, fragment string) bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	for _, event := range p.events {
		if event.Kind == kind && strings.Contains(event.Message, fragment) {
			return true
		}
	}
	return false
}

func (p *player) configure(t *testing.T, worldID string, launch string) {
	t.Helper()
	if err := p.config.UpdateWorld(worldID, func(settings *WorldSettings) {
		settings.SavePath = p.savePath
		settings.Launch = launch
		settings.Include = "WORLD.*, !*.backup"
		settings.Confirmed = true
	}); err != nil {
		t.Fatal(err)
	}
}

func (p *player) write(t *testing.T, name string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(p.savePath, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (p *player) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(p.savePath, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func (p *player) appendCommand(text string, before string, after string) string {
	return "sleep " + before + "; printf '" + text + "' >> '" + filepath.Join(p.savePath, "world.sav") + "'; sleep " + after
}

type group struct {
	server  *testServer
	players []*player
	world   proto.World
}

func newGroup(t *testing.T, names ...string) *group {
	t.Helper()
	server := newTestServer(t)
	ctx := context.Background()
	anonymous := NewAPIClient(server.url, "")
	result := &group{server: server}
	var owner *APIClient
	for index, name := range names {
		var session proto.SessionResponse
		var err error
		if index == 0 {
			session, err = anonymous.CreateGroup(ctx, "friends", name)
			if err == nil {
				owner = NewAPIClient(server.url, session.Token)
			}
		} else {
			invite, inviteErr := owner.CreateInvite(ctx)
			if inviteErr != nil {
				t.Fatal(inviteErr)
			}
			session, err = anonymous.JoinGroup(ctx, invite.Code, name, "PC-"+name)
			if err == nil {
				err = owner.ApproveMember(ctx, session.Member.ID)
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		config, err := LoadConfig(filepath.Join(t.TempDir(), "config"))
		if err != nil {
			t.Fatal(err)
		}
		savePath := filepath.Join(t.TempDir(), "Game", "SaveGames")
		if err := os.MkdirAll(savePath, 0o750); err != nil {
			t.Fatal(err)
		}
		result.players = append(result.players, &player{name: name, client: NewAPIClient(server.url, session.Token), config: config, savePath: savePath})
	}
	world, err := result.players[0].client.CreateWorld(ctx, proto.CreateWorldRequest{Name: "base", GameName: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	result.world = world
	return result
}

func (g *group) forkCount(t *testing.T) int {
	t.Helper()
	revisions, err := g.players[0].client.Revisions(context.Background(), g.world.ID)
	if err != nil {
		t.Fatal(err)
	}
	forks := 0
	for _, revision := range revisions {
		if revision.Branch != proto.MainBranch {
			forks++
		}
	}
	return forks
}

func TestHostHandoffBetweenTwoPlayers(t *testing.T) {
	g := newGroup(t, "a", "b")
	playerA, playerB := g.players[0], g.players[1]
	ctx := context.Background()

	playerA.write(t, "world.sav", "day1;")
	playerA.write(t, "world.sav.backup", "excluded")
	playerA.write(t, "character.sav", "a's character")
	playerB.write(t, "character.sav", "b's character")
	playerB.write(t, "World.sav", "b's unrelated old world")

	playerA.configure(t, g.world.ID, playerA.appendCommand("a-played;", "0.6", "0.6"))
	hostingA := make(chan error, 1)
	go func() { hostingA <- playerA.session(g.world.ID).Host(ctx) }()
	time.Sleep(300 * time.Millisecond)

	playerB.configure(t, g.world.ID, "true")
	refusal := playerB.session(g.world.ID).Host(ctx)
	if lease, held := IsLeaseHeld(refusal); !held || lease.HolderName != "a" {
		t.Fatalf("b hosting while a hosts: %v", refusal)
	}
	if got := playerB.read(t, "World.sav"); got != "b's unrelated old world" {
		t.Fatalf("refused host attempt touched b's save: %q", got)
	}
	if err := <-hostingA; err != nil {
		t.Fatalf("a hosting: %v", err)
	}

	playerB.configure(t, g.world.ID, playerB.appendCommand("b-played;", "0", "0.3"))
	if err := playerB.session(g.world.ID).Host(ctx); err != nil {
		t.Fatalf("b hosting: %v", err)
	}
	if got := playerB.read(t, "world.sav"); got != "day1;a-played;b-played;" {
		t.Fatalf("b world = %q", got)
	}
	if _, err := os.Stat(filepath.Join(playerB.savePath, "World.sav")); err == nil {
		t.Fatal("b's old world with different letter case survived next to the downloaded one")
	}
	if got := playerB.read(t, "character.sav"); got != "b's character" {
		t.Fatalf("b character file was touched: %q", got)
	}
	if _, err := os.Stat(filepath.Join(playerB.savePath, "world.sav.backup")); err == nil {
		t.Fatal("excluded backup file was synced to b")
	}
	backups, _ := filepath.Glob(filepath.Join(playerB.config.BackupDir(g.world.ID), "*", "World.sav"))
	if len(backups) != 1 {
		t.Fatalf("b's never-synced world was not backed up: %v", backups)
	}
	if !playerB.sawEvent(EventWarning, "fork/b/") || g.forkCount(t) != 1 {
		t.Fatal("b's never-synced world was not also kept on the server as a branch")
	}

	playerA.write(t, "world.sav", "day1;a-played;a-offline;")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(ctx); err != nil {
		t.Fatalf("a hosting again: %v", err)
	}
	if got := playerA.read(t, "world.sav"); got != "day1;a-played;b-played;" {
		t.Fatalf("a world after re-host = %q", got)
	}
	if !playerA.sawEvent(EventWarning, "fork/a/") {
		t.Fatal("a's offline progress was not reported as a separate branch")
	}
	if backups, _ := filepath.Glob(filepath.Join(playerA.config.BackupDir(g.world.ID), "*")); len(backups) != 0 {
		t.Fatalf("a's progress is already on the server as a branch, a local backup copy is wasted disk: %v", backups)
	}
	if forks := g.forkCount(t); forks != 2 {
		t.Fatalf("fork revisions = %d", forks)
	}
	status, err := playerA.client.WorldStatus(ctx, g.world.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Lease != nil {
		t.Fatalf("lease still held after all sessions ended: %+v", status.Lease)
	}
}

func TestLeaseSurvivesAnUploadLongerThanTheLeaseTTL(t *testing.T) {
	g := newGroup(t, "a", "b")
	playerA, playerB := g.players[0], g.players[1]
	g.server.store.LeaseTTL = 700 * time.Millisecond
	g.server.uploadDelay.Store(int64(2 * time.Second))
	playerA.write(t, "world.sav", "big world")
	playerA.configure(t, g.world.ID, "true")

	hosting := make(chan error, 1)
	go func() { hosting <- playerA.session(g.world.ID).Host(context.Background()) }()
	time.Sleep(1500 * time.Millisecond)
	if _, err := playerB.client.AcquireLease(context.Background(), g.world.ID, "other"); err == nil {
		t.Fatal("b took the lease while a's upload was still running")
	}
	if err := <-hosting; err != nil {
		t.Fatal(err)
	}
	status, _ := playerA.client.WorldStatus(context.Background(), g.world.ID)
	if status.Head == nil || status.Head.Branch != proto.MainBranch || g.forkCount(t) != 0 {
		t.Fatalf("slow upload did not land on main: %+v", status.Head)
	}
}

func TestRestartedHostKeepsProgressAndStaysOnMain(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	ctx := context.Background()
	playerA.write(t, "world.sav", "start;")
	playerA.configure(t, g.world.ID, playerA.appendCommand("checkpointed;", "0", "0.9"))

	frozen := playerA.session(g.world.ID)
	frozen.Timings.Settle = time.Hour
	var unplugged atomic.Bool
	frozen.Client = NewAPIClient(g.server.url, playerA.client.Token)
	frozen.Client.HTTP = &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		if unplugged.Load() {
			return nil, errors.New("this window crashed")
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	go frozen.Host(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !playerA.sawEvent(EventUploaded, CheckpointNote) || !playerA.sawEvent(EventGameExited, "") {
		if time.Now().After(deadline) {
			t.Fatal("the first session never uploaded a checkpoint and closed its game")
		}
		time.Sleep(20 * time.Millisecond)
	}
	unplugged.Store(true)
	playerA.write(t, "world.sav", "start;checkpointed;played-after-checkpoint;")

	tooSoon := playerA.session(g.world.ID)
	if err := tooSoon.Host(ctx); err == nil || !strings.Contains(err.Error(), "another peerly window") {
		t.Fatalf("a second window right after the crash should be told to wait: %v", err)
	}
	g.server.store.Now = func() time.Time { return time.Now().Add(store.ActiveWindow + time.Second) }

	playerA.configure(t, g.world.ID, "true")
	restarted := playerA.session(g.world.ID)
	restarted.LocalChanges = LocalChangesLatest
	if err := restarted.Host(ctx); err != nil {
		t.Fatal(err)
	}
	if got := playerA.read(t, "world.sav"); got != "start;checkpointed;played-after-checkpoint;" {
		t.Fatalf("restart rolled the local world back to %q", got)
	}
	if forks := g.forkCount(t); forks != 0 {
		t.Fatalf("restart after a crash produced %d branches", forks)
	}
}

func TestStopWhilePlayingUploadsAndFreesTheWorld(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	playerA.write(t, "world.sav", "start;")
	playerA.configure(t, g.world.ID, playerA.appendCommand("played;", "0", "4"))
	session := playerA.session(g.world.ID)
	session.Timings.Checkpoint = time.Hour
	hosting := make(chan error, 1)
	go func() { hosting <- session.Host(context.Background()) }()
	time.Sleep(600 * time.Millisecond)
	session.Stop()
	select {
	case err := <-hosting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not end the session")
	}
	status, _ := playerA.client.WorldStatus(context.Background(), g.world.ID)
	if status.Lease != nil || status.Head == nil || status.Head.Note != "session end" {
		t.Fatalf("after stop: lease=%v head=%+v", status.Lease, status.Head)
	}
}

func TestFinalUploadRetriesUntilTheServerIsBack(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	playerA.write(t, "world.sav", "start;")
	playerA.configure(t, g.world.ID, playerA.appendCommand("played;", "0", "0.2"))
	session := playerA.session(g.world.ID)
	session.Timings.Checkpoint = time.Hour
	working := session.Client
	var failures atomic.Int32
	failing := NewAPIClient(g.server.url, working.Token)
	failing.HTTP = &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/revisions") && strings.Contains(request.URL.RawQuery, "session+end") && failures.Add(1) <= 3 {
			return nil, errors.New("simulated network outage")
		}
		return http.DefaultTransport.RoundTrip(request)
	})}
	session.Client = failing
	if err := session.Host(context.Background()); err != nil {
		t.Fatal(err)
	}
	if failures.Load() < 4 || !playerA.sawEvent(EventWarning, "trying again") {
		t.Fatalf("expected retries, saw %d attempts", failures.Load())
	}
	status, _ := working.WorldStatus(context.Background(), g.world.ID)
	if status.Head == nil || status.Head.Note != "session end" {
		t.Fatalf("progress never reached the server: %+v", status.Head)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestSyncOnlyNeverStartsTheGame(t *testing.T) {
	g := newGroup(t, "a", "b")
	playerA, playerB := g.players[0], g.players[1]
	marker := filepath.Join(t.TempDir(), "game-was-started")
	playerA.write(t, "world.sav", "from a;")
	playerA.configure(t, g.world.ID, "touch '"+marker+"'")
	sync := playerA.session(g.world.ID)
	sync.SyncOnly = true
	if err := sync.Host(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("sync started the game")
	}
	playerB.configure(t, g.world.ID, "")
	pull := playerB.session(g.world.ID)
	pull.SyncOnly = true
	if err := pull.Host(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := playerB.read(t, "world.sav"); got != "from a;" {
		t.Fatalf("b after sync = %q", got)
	}
}

func TestHostRefusesUnsafeOrIncompleteSetups(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	ctx := context.Background()
	host := func() error { return playerA.session(g.world.ID).Host(ctx) }

	if err := host(); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("unconfirmed settings: %v", err)
	}
	playerA.configure(t, g.world.ID, "steam://rungameid/892970")
	if err := host(); err == nil || !strings.Contains(err.Error(), "process name") {
		t.Fatalf("launcher link without process name: %v", err)
	}
	playerA.configure(t, g.world.ID, "")
	if err := host(); err == nil || !strings.Contains(err.Error(), "launch command or the game's process name") {
		t.Fatalf("no way to detect the end of the session: %v", err)
	}
	playerA.configure(t, g.world.ID, "true")
	playerA.config.UpdateWorld(g.world.ID, func(settings *WorldSettings) {
		settings.SavePath = filepath.Join(playerA.savePath, "missing", "deeper")
	})
	if err := host(); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing folder: %v", err)
	}
	home, _ := os.UserHomeDir()
	playerA.config.UpdateWorld(g.world.ID, func(settings *WorldSettings) { settings.SavePath = filepath.Join(home, "Documents") })
	if err := host(); err == nil || !strings.Contains(err.Error(), "not a game save folder") {
		t.Fatalf("documents folder: %v", err)
	}
	status, _ := playerA.client.WorldStatus(ctx, g.world.ID)
	if status.Lease != nil {
		t.Fatal("a refused setup still took the lease")
	}
}

func TestSessionSaysSoWhenNoFileMatchesTheFilter(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	playerA.write(t, "MyRealWorld.sav", "progress")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !playerA.sawEvent(EventWarning, "nothing was uploaded") {
		t.Fatal("a session that shared nothing ended without a warning")
	}
}

func TestUnconfirmedLocalProgressNeverOverwritesTheGroupsWorld(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	ctx := context.Background()
	playerA.write(t, "world.sav", "group world;")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(ctx); err != nil {
		t.Fatal(err)
	}
	playerA.write(t, "world.sav", "something else entirely")
	if err := playerA.session(g.world.ID).Host(ctx); err != nil {
		t.Fatal(err)
	}
	if got := playerA.read(t, "world.sav"); got != "group world;" || g.forkCount(t) != 1 {
		t.Fatalf("without confirmation local changes must become a branch: local=%q forks=%d", got, g.forkCount(t))
	}
}

func TestPointingAtAnotherFolderIsNotTreatedAsProgress(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	ctx := context.Background()
	playerA.write(t, "world.sav", "group world;")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(ctx); err != nil {
		t.Fatal(err)
	}
	otherFolder := filepath.Join(t.TempDir(), "OtherGame", "Saves")
	os.MkdirAll(otherFolder, 0o750)
	os.WriteFile(filepath.Join(otherFolder, "world.sav"), []byte("ancient solo test world"), 0o644)
	playerA.savePath = otherFolder
	playerA.configure(t, g.world.ID, "true")
	session := playerA.session(g.world.ID)
	session.LocalChanges = LocalChangesLatest
	if err := session.Host(ctx); err != nil {
		t.Fatal(err)
	}
	status, _ := playerA.client.WorldStatus(ctx, g.world.ID)
	if got := playerA.read(t, "world.sav"); got != "group world;" || status.Head.Note == "progress made on this PC since the last sync" {
		t.Fatalf("an unrelated folder replaced the group's world: local=%q head=%+v", got, status.Head)
	}
	if g.forkCount(t) != 1 {
		t.Fatalf("the unrelated world should be kept as a branch, forks=%d", g.forkCount(t))
	}
}

func TestLostUploadResponseDoesNotSplitTheSession(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	playerA.write(t, "world.sav", "day1;")
	playerA.configure(t, g.world.ID, playerA.appendCommand("cp1;", "0", "0.8")+"; printf 'final;' >> '"+filepath.Join(playerA.savePath, "world.sav")+"'")
	session := playerA.session(g.world.ID)
	var dropped atomic.Bool
	lossy := NewAPIClient(g.server.url, session.Client.Token)
	lossy.HTTP = &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil && strings.Contains(request.URL.RawQuery, "note="+CheckpointNote) && dropped.CompareAndSwap(false, true) {
			response.Body.Close()
			return nil, errors.New("connection reset after the server already saved the upload")
		}
		return response, err
	})}
	session.Client = lossy
	if err := session.Host(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !dropped.Load() {
		t.Fatal("the test never dropped a response")
	}
	status, _ := playerA.client.WorldStatus(context.Background(), g.world.ID)
	if g.forkCount(t) != 0 || status.Head == nil || status.Head.Note != "session end" {
		t.Fatalf("a dropped response split the host's own session: forks=%d head=%+v", g.forkCount(t), status.Head)
	}
}

func TestGameStartedDuringTheDownloadStopsTheSwap(t *testing.T) {
	g := newGroup(t, "a", "b")
	playerA, playerB := g.players[0], g.players[1]
	ctx := context.Background()
	playerA.write(t, "world.sav", "group world;")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(ctx); err != nil {
		t.Fatal(err)
	}
	sleepBinary, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	fakeGame := filepath.Join(t.TempDir(), "fakegame-peerly")
	raw, _ := os.ReadFile(sleepBinary)
	os.WriteFile(fakeGame, raw, 0o755)
	var game *exec.Cmd
	startGame := func() {
		if game == nil {
			game = exec.Command(fakeGame, "20")
			game.Start()
			time.Sleep(300 * time.Millisecond)
		}
	}
	g.server.onDownload.Store(&startGame)
	t.Cleanup(func() {
		if game != nil {
			game.Process.Kill()
			game.Wait()
		}
	})
	playerB.write(t, "world.sav", "b's local files")
	playerB.config.UpdateWorld(g.world.ID, func(settings *WorldSettings) {
		settings.SavePath, settings.Include, settings.Process, settings.Confirmed = playerB.savePath, "world.*", "fakegame-peerly", true
	})
	err = playerB.session(g.world.ID).Host(ctx)
	if err == nil || !strings.Contains(err.Error(), "is running") {
		t.Fatalf("host continued although the game was started mid-download: %v", err)
	}
	if got := playerB.read(t, "world.sav"); got != "b's local files" {
		t.Fatalf("files were swapped under a running game: %q", got)
	}
	status, _ := playerB.client.WorldStatus(ctx, g.world.ID)
	if status.Lease != nil {
		t.Fatal("aborted host kept the lease")
	}
}

func TestUploadsWorkThroughAnHTTPSRedirect(t *testing.T) {
	g := newGroup(t, "a")
	playerA := g.players[0]
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, g.server.url+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	defer redirector.Close()
	viaRedirect := NewAPIClient(redirector.URL, playerA.client.Token)
	resolved, err := viaRedirect.ResolveBaseURL(context.Background())
	if err != nil || resolved != g.server.url {
		t.Fatalf("resolved %q, %v, want %q", resolved, err, g.server.url)
	}
	playerA.client = viaRedirect
	playerA.write(t, "world.sav", "day1;")
	playerA.configure(t, g.world.ID, "true")
	if err := playerA.session(g.world.ID).Host(context.Background()); err != nil {
		t.Fatalf("hosting through a redirecting address: %v", err)
	}
}
