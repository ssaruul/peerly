package ui

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"peerly/client/core"
	"peerly/proto"
)

//go:embed web
var webFiles embed.FS

const keptEvents = 200

type TimedEvent struct {
	At      int64          `json:"at"`
	Kind    core.EventKind `json:"kind"`
	Message string         `json:"message"`
}

type Progress struct {
	Label string `json:"label"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
}

type HostingState struct {
	WorldID     string       `json:"world_id"`
	WorldName   string       `json:"world_name"`
	Active      bool         `json:"active"`
	SyncOnly    bool         `json:"sync_only"`
	Phase       core.Phase   `json:"phase"`
	GameRunning bool         `json:"game_running"`
	Progress    *Progress    `json:"progress"`
	Error       string       `json:"error"`
	Events      []TimedEvent `json:"events"`
}

type WorldView struct {
	Status    proto.WorldStatus  `json:"status"`
	Local     core.WorldSettings `json:"local"`
	Resolved  core.Resolved      `json:"resolved"`
	CanDelete bool               `json:"can_delete"`
	BackupDir string             `json:"backup_dir"`
}

type State struct {
	Configured  bool           `json:"configured"`
	ServerURL   string         `json:"server_url"`
	Group       proto.Group    `json:"group"`
	Member      proto.Member   `json:"member"`
	Members     []proto.Member `json:"members"`
	IsOwner     bool           `json:"is_owner"`
	Pending     bool           `json:"pending"`
	Worlds      []WorldView    `json:"worlds"`
	Hosting     HostingState   `json:"hosting"`
	ServerError string         `json:"server_error"`
	AccessLost  bool           `json:"access_lost"`
	Notice      string         `json:"notice"`
	Presets     []Preset       `json:"presets"`
}

type App struct {
	AccessKey string

	config  *core.ConfigFile
	mutex   sync.Mutex
	hosting HostingState
	session *core.Session
	done    chan struct{}
	pulling bool

	cachedStatuses []proto.WorldStatus
	cachedMembers  []proto.Member
	cachedGroup    proto.Group
}

func New(config *core.ConfigFile) *App {
	return &App{config: config, hosting: HostingState{Phase: core.PhaseIdle, Events: []TimedEvent{}}}
}

func (a *App) client() *core.APIClient {
	saved := a.config.Snapshot()
	return core.NewAPIClient(saved.ServerURL, saved.Token)
}

func (a *App) Handler() http.Handler {
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", noCache(http.FileServerFS(static)))
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"app": "peerly"})
	})
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/create-group", a.createGroup)
	mux.HandleFunc("POST /api/join", a.joinGroup)
	mux.HandleFunc("POST /api/leave", a.leave)
	mux.HandleFunc("POST /api/invite", a.createInvite)
	mux.HandleFunc("POST /api/members/{id}/approve", a.approveMember)
	mux.HandleFunc("POST /api/members/{id}/owner", a.transferOwnership)
	mux.HandleFunc("DELETE /api/members/{id}", a.removeMember)
	mux.HandleFunc("POST /api/worlds", a.createWorld)
	mux.HandleFunc("DELETE /api/worlds/{id}", a.deleteWorld)
	mux.HandleFunc("POST /api/worlds/{id}/preview", a.preview)
	mux.HandleFunc("GET /api/worlds/{id}/precheck", a.precheck)
	mux.HandleFunc("PUT /api/worlds/{id}/settings", a.saveSettings)
	mux.HandleFunc("POST /api/worlds/{id}/host", a.host)
	mux.HandleFunc("POST /api/worlds/{id}/sync", a.host)
	mux.HandleFunc("POST /api/host/stop", a.stopHosting)
	mux.HandleFunc("PUT /api/worlds/{id}/join-info", a.setJoinInfo)
	mux.HandleFunc("GET /api/worlds/{id}/revisions", a.revisions)
	mux.HandleFunc("POST /api/worlds/{id}/promote", a.promote)
	mux.HandleFunc("POST /api/worlds/{id}/pull", a.pull)
	mux.HandleFunc("DELETE /api/revisions/{id}", a.discard)
	return a.guard(mux)
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if cut := strings.LastIndex(host, ":"); cut > 0 && !strings.HasSuffix(host, "]") {
			host = host[:cut]
		}
		switch strings.ToLower(host) {
		case "127.0.0.1", "localhost", "wails.localhost", "wails", "[::1]":
		default:
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if fetchSite := r.Header.Get("Sec-Fetch-Site"); fetchSite != "" && fetchSite != "same-origin" && fetchSite != "none" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Peerly") != "1" {
			http.Error(w, "missing X-Peerly header", http.StatusForbidden)
			return
		}
		isAPI := strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/ping"
		if a.AccessKey != "" && isAPI && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Peerly-Key")), []byte(a.AccessKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "This page was opened without its access key. Close the tab and open peerly from its shortcut again."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func friendly(err error) string {
	if lease, held := core.IsLeaseHeld(err); held {
		return lease.HolderName + " is hosting this world right now"
	}
	if core.IsUnauthorized(err) {
		return "the server no longer recognizes this PC. Leave the group on this PC and join again with an invite code"
	}
	if core.IsPending(err) {
		return "the group owner has not approved this PC yet"
	}
	var apiError *core.APIError
	if errors.As(err, &apiError) {
		return apiError.Message
	}
	message := err.Error()
	for _, marker := range []string{"no such host", "connection refused", "i/o timeout", "deadline exceeded", "network is unreachable", "certificate"} {
		if strings.Contains(message, marker) {
			return "the server cannot be reached (" + marker + "). Check the server address and your internet connection"
		}
	}
	return message
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var apiError *core.APIError
	if errors.As(err, &apiError) {
		status = apiError.Status
	}
	writeJSON(w, status, map[string]string{"error": friendly(err)})
}

func refuse(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		refuse(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func (a *App) hostingSnapshot() HostingState {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	snapshot := a.hosting
	snapshot.Events = append([]TimedEvent{}, a.hosting.Events...)
	if a.hosting.Progress != nil {
		progress := *a.hosting.Progress
		snapshot.Progress = &progress
	}
	return snapshot
}

func (a *App) HostingActive() bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.hosting.Active
}

func (a *App) busyWith(worldID string) bool {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	return a.hosting.Active && a.hosting.WorldID == worldID
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	saved := a.config.Snapshot()
	state := State{
		Configured: saved.Token != "",
		ServerURL:  saved.ServerURL,
		Group:      saved.Group,
		Member:     saved.Member,
		Members:    []proto.Member{},
		Worlds:     []WorldView{},
		Hosting:    a.hostingSnapshot(),
		Notice:     a.config.Notice,
		Presets:    Presets,
	}
	if state.Configured {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		client := a.client()
		me, err := client.Me(ctx)
		var statuses []proto.WorldStatus
		if err == nil && me.Member.Status == proto.MemberApproved {
			statuses, err = client.Worlds(ctx)
		}
		if err == nil && me.Member.Status != saved.Member.Status {
			a.config.Update(func(stored *core.Config) { stored.Member = me.Member })
			state.Member = me.Member
		}
		a.mutex.Lock()
		if err == nil {
			a.cachedStatuses, a.cachedMembers, a.cachedGroup = statuses, me.Members, me.Group
		} else {
			state.ServerError = friendly(err)
			state.AccessLost = core.IsUnauthorized(err)
		}
		statuses, members, group := a.cachedStatuses, a.cachedMembers, a.cachedGroup
		a.mutex.Unlock()
		if group.ID != "" {
			state.Group = group
		}
		state.Members = append(state.Members, members...)
		state.Pending = state.Member.Status == proto.MemberPending
		state.IsOwner = state.Group.OwnerID != "" && state.Group.OwnerID == saved.Member.ID
		for _, status := range statuses {
			local := a.config.World(status.World.ID)
			state.Worlds = append(state.Worlds, WorldView{
				Status:    status,
				Local:     local,
				Resolved:  core.ResolveSettings(status.World, local),
				CanDelete: state.IsOwner || status.World.CreatedBy == saved.Member.ID,
				BackupDir: a.config.BackupDir(status.World.ID),
			})
		}
	}
	writeJSON(w, http.StatusOK, state)
}

type sessionRequest struct {
	ServerURL   string `json:"server_url"`
	AdminKey    string `json:"admin_key"`
	Name        string `json:"name"`
	InviteCode  string `json:"invite_code"`
	DisplayName string `json:"display_name"`
}

func (a *App) openSession(w http.ResponseWriter, r *http.Request, creating bool) {
	request := sessionRequest{}
	if !decode(w, r, &request) {
		return
	}
	serverURL, err := core.NormalizeServerURL(request.ServerURL)
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	client := core.NewAPIClient(serverURL, "")
	client.AdminKey = strings.TrimSpace(request.AdminKey)
	resolvedURL, err := client.ResolveBaseURL(ctx)
	if err != nil {
		refuse(w, http.StatusBadGateway, "No peerly server answers at "+serverURL+": "+friendly(err))
		return
	}
	serverURL = resolvedURL
	client.BaseURL = resolvedURL
	var session proto.SessionResponse
	if creating {
		session, err = client.CreateGroup(ctx, request.Name, request.DisplayName)
	} else {
		deviceName, _ := os.Hostname()
		session, err = client.JoinGroup(ctx, request.InviteCode, request.DisplayName, deviceName)
	}
	if err != nil {
		fail(w, err)
		return
	}
	a.mutex.Lock()
	a.cachedStatuses, a.cachedMembers, a.cachedGroup = nil, nil, proto.Group{}
	a.mutex.Unlock()
	a.config.Notice = ""
	err = a.config.Update(func(stored *core.Config) {
		stored.ServerURL = serverURL
		stored.Token = session.Token
		stored.Member = session.Member
		stored.Group = session.Group
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": session.Member.Status})
}

func (a *App) createGroup(w http.ResponseWriter, r *http.Request) {
	a.openSession(w, r, true)
}

func (a *App) joinGroup(w http.ResponseWriter, r *http.Request) {
	a.openSession(w, r, false)
}

func (a *App) leave(w http.ResponseWriter, r *http.Request) {
	if a.HostingActive() {
		refuse(w, http.StatusConflict, "stop hosting before leaving the group")
		return
	}
	a.mutex.Lock()
	a.cachedStatuses, a.cachedMembers, a.cachedGroup = nil, nil, proto.Group{}
	a.mutex.Unlock()
	err := a.config.Update(func(stored *core.Config) {
		stored.Token = ""
		stored.Member = proto.Member{}
		stored.Group = proto.Group{}
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) createInvite(w http.ResponseWriter, r *http.Request) {
	invite, err := a.client().CreateInvite(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, invite)
}

func (a *App) transferOwnership(w http.ResponseWriter, r *http.Request) {
	if err := a.client().TransferOwnership(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	a.mutex.Lock()
	a.cachedGroup = proto.Group{}
	a.mutex.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) approveMember(w http.ResponseWriter, r *http.Request) {
	if err := a.client().ApproveMember(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) removeMember(w http.ResponseWriter, r *http.Request) {
	if err := a.client().RemoveMember(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) createWorld(w http.ResponseWriter, r *http.Request) {
	request := proto.CreateWorldRequest{}
	if !decode(w, r, &request) {
		return
	}
	world, err := a.client().CreateWorld(r.Context(), request)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, world)
}

func (a *App) deleteWorld(w http.ResponseWriter, r *http.Request) {
	if a.busyWith(r.PathValue("id")) {
		refuse(w, http.StatusConflict, "stop hosting this world before deleting it")
		return
	}
	if err := a.client().DeleteWorld(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type settingsRequest struct {
	SavePath          string `json:"save_path"`
	Launch            string `json:"launch"`
	Process           string `json:"process"`
	Include           string `json:"include"`
	CheckpointMinutes int    `json:"checkpoint_minutes"`
}

func (a *App) preview(w http.ResponseWriter, r *http.Request) {
	request := settingsRequest{}
	if !decode(w, r, &request) {
		return
	}
	writeJSON(w, http.StatusOK, core.SurveyFolder(core.ExpandPath(request.SavePath), core.ParseInclude(request.Include)))
}

type precheckResponse struct {
	Survey          core.Survey `json:"survey"`
	NeverSynced     bool        `json:"never_synced"`
	GroupHasSave    bool        `json:"group_has_save"`
	Include         string      `json:"include"`
	LocalProgress   bool        `json:"local_progress"`
	GroupMovedOn    bool        `json:"group_moved_on"`
	LastSavedByName string      `json:"last_saved_by_name"`
}

func (a *App) precheck(w http.ResponseWriter, r *http.Request) {
	worldID := r.PathValue("id")
	status, err := a.client().WorldStatus(r.Context(), worldID)
	if err != nil {
		fail(w, err)
		return
	}
	local := a.config.World(worldID)
	response := precheckResponse{
		Survey:       core.SurveyFolder(core.ExpandPath(local.SavePath), core.ParseInclude(local.Include)),
		NeverSynced:  !local.SyncedBefore(),
		GroupHasSave: status.Head != nil,
		Include:      local.Include,
	}
	if response.Survey.Problem == "" {
		pending, err := core.LocalProgressPending(a.config, worldID)
		if err != nil {
			refuse(w, http.StatusUnprocessableEntity, "The world files on this PC could not be read: "+err.Error())
			return
		}
		response.LocalProgress = pending
	}
	if status.Head != nil {
		response.GroupMovedOn = status.Head.ID != local.LastRevisionID
		response.LastSavedByName = status.Head.AuthorName
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	worldID := r.PathValue("id")
	request := settingsRequest{}
	if !decode(w, r, &request) {
		return
	}
	if a.busyWith(worldID) {
		refuse(w, http.StatusConflict, "settings cannot change while this world is being hosted from this PC")
		return
	}
	survey := core.SurveyFolder(core.ExpandPath(request.SavePath), core.ParseInclude(request.Include))
	if survey.Problem != "" {
		refuse(w, http.StatusUnprocessableEntity, survey.Problem)
		return
	}
	process := strings.TrimSpace(request.Process)
	launch := strings.TrimSpace(request.Launch)
	if process == "" && core.IsLink(launch) {
		refuse(w, http.StatusUnprocessableEntity, "A steam:// or launcher link returns immediately, so the game's process name is needed too (for example valheim.exe). Look it up in Task Manager, Details tab, while the game runs")
		return
	}
	err := a.config.UpdateWorld(worldID, func(settings *core.WorldSettings) {
		settings.SavePath = strings.TrimSpace(request.SavePath)
		settings.Launch = launch
		settings.Process = process
		settings.Include = strings.TrimSpace(request.Include)
		settings.CheckpointMinutes = request.CheckpointMinutes
		settings.Confirmed = true
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, survey)
}

func (a *App) record(event core.Event) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	switch event.Kind {
	case core.EventPhase:
		a.hosting.Phase = event.Phase
		return
	case core.EventProgress:
		if event.Message == "" {
			a.hosting.Progress = nil
		} else {
			a.hosting.Progress = &Progress{Label: event.Message, Done: event.Done, Total: event.Total}
		}
		return
	case core.EventGameStarted:
		a.hosting.GameRunning = true
	case core.EventGameExited:
		a.hosting.GameRunning = false
	}
	a.hosting.Events = append(a.hosting.Events, TimedEvent{At: time.Now().UnixMilli(), Kind: event.Kind, Message: event.Message})
	if len(a.hosting.Events) > keptEvents {
		a.hosting.Events = a.hosting.Events[len(a.hosting.Events)-keptEvents:]
	}
}

func (a *App) host(w http.ResponseWriter, r *http.Request) {
	worldID := r.PathValue("id")
	syncOnly := strings.HasSuffix(r.URL.Path, "/sync")
	request := struct {
		LocalChanges core.LocalChangesPolicy `json:"local_changes"`
	}{}
	if r.ContentLength != 0 && !decode(w, r, &request) {
		return
	}
	client := a.client()
	status, err := client.WorldStatus(r.Context(), worldID)
	if err != nil {
		fail(w, err)
		return
	}
	a.mutex.Lock()
	if a.hosting.Active || a.pulling {
		busyWith := a.hosting.WorldName
		if a.pulling {
			busyWith = "copying a save to this PC, wait until that is done"
		}
		a.mutex.Unlock()
		refuse(w, http.StatusConflict, "this PC is already busy with "+busyWith)
		return
	}
	session := &core.Session{Client: client, Config: a.config, WorldID: worldID, Timings: core.DefaultTimings(), OnEvent: a.record, SyncOnly: syncOnly, LocalChanges: request.LocalChanges}
	done := make(chan struct{})
	a.hosting = HostingState{WorldID: worldID, WorldName: status.World.Name, Active: true, SyncOnly: syncOnly, Phase: core.PhasePreparing, Events: []TimedEvent{}}
	a.session = session
	a.done = done
	a.mutex.Unlock()

	go func() {
		defer close(done)
		err := session.Host(context.Background())
		a.mutex.Lock()
		defer a.mutex.Unlock()
		a.hosting.Active = false
		a.hosting.GameRunning = false
		a.hosting.Progress = nil
		a.hosting.Phase = core.PhaseIdle
		a.session = nil
		if err != nil {
			a.hosting.Error = friendly(err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (a *App) stopHosting(w http.ResponseWriter, r *http.Request) {
	a.mutex.Lock()
	session := a.session
	a.mutex.Unlock()
	if session != nil {
		session.Stop()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) StopAndWait(timeout time.Duration) {
	a.mutex.Lock()
	session, done := a.session, a.done
	a.mutex.Unlock()
	if session == nil {
		return
	}
	session.Stop()
	select {
	case <-done:
	case <-time.After(timeout):
		session.Stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
}

func (a *App) setJoinInfo(w http.ResponseWriter, r *http.Request) {
	request := struct {
		JoinInfo string `json:"join_info"`
	}{}
	if !decode(w, r, &request) {
		return
	}
	a.mutex.Lock()
	session := a.session
	a.mutex.Unlock()
	if session == nil || session.Lease().FencingToken == 0 {
		refuse(w, http.StatusConflict, "join info can be shared only while you are hosting")
		return
	}
	if err := a.client().SetJoinInfo(r.Context(), r.PathValue("id"), session.Lease().FencingToken, request.JoinInfo); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) revisions(w http.ResponseWriter, r *http.Request) {
	revisions, err := a.client().Revisions(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

type revisionRequest struct {
	RevisionID string `json:"revision_id"`
}

func (a *App) promote(w http.ResponseWriter, r *http.Request) {
	request := revisionRequest{}
	if !decode(w, r, &request) {
		return
	}
	revision, err := a.client().Promote(r.Context(), r.PathValue("id"), request.RevisionID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revision)
}

func (a *App) pull(w http.ResponseWriter, r *http.Request) {
	request := revisionRequest{}
	if !decode(w, r, &request) {
		return
	}
	a.mutex.Lock()
	if a.hosting.Active || a.pulling {
		a.mutex.Unlock()
		refuse(w, http.StatusConflict, "local files cannot be replaced while this PC is hosting, syncing or already copying a save")
		return
	}
	a.pulling = true
	a.mutex.Unlock()
	defer func() {
		a.mutex.Lock()
		a.pulling = false
		a.mutex.Unlock()
	}()
	client := a.client()
	status, err := client.WorldStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	result, err := core.RestoreRevision(context.WithoutCancel(r.Context()), client, a.config, status.World, request.RevisionID, core.RestoreOptions{})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup_dir": result.BackupDir, "skipped": result.Skipped})
}

func (a *App) discard(w http.ResponseWriter, r *http.Request) {
	if err := a.client().DiscardFork(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
