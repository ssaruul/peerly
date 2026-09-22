package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"peerly/proto"
)

type EventKind string

const (
	EventInfo        EventKind = "info"
	EventWarning     EventKind = "warning"
	EventLeased      EventKind = "leased"
	EventRestored    EventKind = "restored"
	EventGameStarted EventKind = "game_started"
	EventGameExited  EventKind = "game_exited"
	EventUploaded    EventKind = "uploaded"
	EventReleased    EventKind = "released"
	EventPhase       EventKind = "phase"
	EventProgress    EventKind = "progress"
)

type Phase string

const (
	PhaseIdle      Phase = "idle"
	PhasePreparing Phase = "preparing"
	PhasePlaying   Phase = "playing"
	PhaseSaving    Phase = "saving"
)

const CheckpointNote = "checkpoint"

var ErrNotConfirmed = errors.New("check the folder and files for this world on this PC first (Settings), then host")

type Event struct {
	Kind     EventKind       `json:"kind"`
	Message  string          `json:"message"`
	Revision *proto.Revision `json:"revision,omitempty"`
	Phase    Phase           `json:"phase,omitempty"`
	Done     int64           `json:"done,omitempty"`
	Total    int64           `json:"total,omitempty"`
}

type Timings struct {
	Heartbeat       time.Duration
	Watch           time.Duration
	Quiet           time.Duration
	Settle          time.Duration
	ProcessPoll     time.Duration
	ProcessReminder time.Duration
	UploadRetry     time.Duration
	Checkpoint      time.Duration
}

func DefaultTimings() Timings {
	return Timings{
		Heartbeat:       30 * time.Second,
		Watch:           5 * time.Second,
		Quiet:           20 * time.Second,
		Settle:          5 * time.Second,
		ProcessPoll:     3 * time.Second,
		ProcessReminder: 5 * time.Minute,
		UploadRetry:     15 * time.Second,
	}
}

type LocalChangesPolicy string

const (
	LocalChangesBranch LocalChangesPolicy = "branch"
	LocalChangesLatest LocalChangesPolicy = "latest"
)

type Session struct {
	Client       *APIClient
	Config       *ConfigFile
	WorldID      string
	OnEvent      func(Event)
	Timings      Timings
	SyncOnly     bool
	LocalChanges LocalChangesPolicy

	world               proto.World
	settings            WorldSettings
	folder              string
	filter              IncludeFilter
	lease               proto.Lease
	leaseLost           atomic.Bool
	parentID            string
	uploadedFingerprint string
	previousSize        int64
	headIsCheckpoint    bool

	mutex       sync.Mutex
	cancelPhase context.CancelFunc
	stopped     bool
	sessionID   string
}

func newSessionID() string {
	buffer := make([]byte, 8)
	rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func (s *Session) emit(kind EventKind, format string, args ...any) {
	if s.OnEvent != nil {
		s.OnEvent(Event{Kind: kind, Message: fmt.Sprintf(format, args...)})
	}
}

func (s *Session) progress(label string) ProgressFunc {
	return func(done int64, total int64) {
		if s.OnEvent != nil {
			s.OnEvent(Event{Kind: EventProgress, Message: label, Done: done, Total: total})
		}
	}
}

func (s *Session) clearProgress() {
	if s.OnEvent != nil {
		s.OnEvent(Event{Kind: EventProgress})
	}
}

func (s *Session) Lease() proto.Lease {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.lease
}

func (s *Session) Stop() {
	s.mutex.Lock()
	s.stopped = true
	cancel := s.cancelPhase
	s.mutex.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) enter(phase Phase, parent context.Context) (context.Context, context.CancelFunc) {
	phaseContext, cancel := context.WithCancel(parent)
	s.mutex.Lock()
	s.cancelPhase = cancel
	if s.stopped && phase != PhaseSaving {
		cancel()
	}
	s.stopped = false
	s.mutex.Unlock()
	if s.OnEvent != nil {
		s.OnEvent(Event{Kind: EventPhase, Phase: phase})
	}
	return phaseContext, cancel
}

func (s *Session) validate(status proto.WorldStatus) error {
	if err := CheckSaveFolder(s.folder); err != nil {
		return err
	}
	if err := s.filter.Validate(); err != nil {
		return err
	}
	if !s.SyncOnly {
		if s.settings.Launch == "" && s.settings.Process == "" {
			return errors.New("set a launch command or the game's process name in Settings, otherwise the app cannot tell when you stop playing")
		}
		if s.settings.Process == "" && IsLink(s.settings.Launch) {
			return errors.New("a steam:// or launcher link returns immediately, so also set the game's process name in Settings (for example valheim.exe)")
		}
	}
	if _, err := os.Stat(s.folder); errors.Is(err, os.ErrNotExist) {
		if _, parentErr := os.Stat(filepath.Dir(s.folder)); parentErr != nil {
			return fmt.Errorf("the save folder %s does not exist on this PC. Check the folder in Settings, and start the game once if it was never run here", s.folder)
		}
		if status.Head == nil {
			return fmt.Errorf("the save folder %s does not exist yet and the group has no save to download. Create the world in the game first, then host", s.folder)
		}
	}
	if s.settings.Process != "" {
		running, err := processRunning(s.settings.Process)
		if err != nil {
			return err
		}
		if running {
			return fmt.Errorf("%s is running. Close the game first so its save can be synced safely", normalizeProcessName(s.settings.Process))
		}
	}
	return nil
}

func (s *Session) Host(ctx context.Context) error {
	status, err := s.Client.WorldStatus(ctx, s.WorldID)
	if err != nil {
		return err
	}
	s.world = status.World
	s.WorldID = status.World.ID
	s.settings = s.Config.World(s.WorldID)
	if !s.settings.Confirmed {
		return ErrNotConfirmed
	}
	s.folder = ExpandPath(s.settings.SavePath)
	s.filter = ParseInclude(s.settings.Include)
	if err := s.validate(status); err != nil {
		return err
	}
	if status.Head != nil {
		s.previousSize = status.Head.Size
	}

	if s.sessionID == "" {
		s.sessionID = newSessionID()
	}
	lease, err := s.Client.AcquireLease(ctx, s.WorldID, s.sessionID)
	if err != nil {
		return err
	}
	s.mutex.Lock()
	s.lease = lease
	s.mutex.Unlock()
	s.headIsCheckpoint = status.Head != nil && status.Head.ID == lease.BaseRevisionID && status.Head.Note == CheckpointNote
	s.emit(EventLeased, "you hold the host lease for %s", s.world.Name)

	heartbeatContext, stopHeartbeat := context.WithCancel(context.Background())
	heartbeatStopped := make(chan struct{})
	go func() {
		defer close(heartbeatStopped)
		s.heartbeatLoop(heartbeatContext)
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatStopped
		s.clearProgress()
		if s.OnEvent != nil {
			s.OnEvent(Event{Kind: EventPhase, Phase: PhaseIdle})
		}
		if s.leaseLost.Load() {
			return
		}
		releaseContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Client.ReleaseLease(releaseContext, s.WorldID, lease.FencingToken); err != nil {
			s.emit(EventWarning, "could not tell the server you are done, the world frees itself within a few minutes: %v", err)
			return
		}
		s.emit(EventReleased, "done, anyone in the group can host now")
	}()

	prepareContext, endPrepare := s.enter(PhasePreparing, ctx)
	err = s.prepareLocal(prepareContext)
	cancelled := prepareContext.Err() != nil
	endPrepare()
	if err != nil {
		if cancelled {
			return errors.New("cancelled before the game started")
		}
		return err
	}

	if !s.SyncOnly {
		gameContext, endGame := context.WithCancel(context.Background())
		defer endGame()
		gameDone, err := s.startGame(gameContext)
		if err != nil {
			return fmt.Errorf("the game could not be started: %w", err)
		}
		playContext, endPlay := s.enter(PhasePlaying, ctx)
		s.watch(playContext, gameDone)
		endPlay()
	}

	saveContext, endSave := s.enter(PhaseSaving, context.Background())
	defer endSave()
	return s.finalSync(saveContext)
}

func (s *Session) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(s.Timings.Heartbeat)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		beatContext, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := s.Client.Heartbeat(beatContext, s.WorldID, s.lease.FencingToken)
		cancel()
		switch {
		case err == nil:
			if failing {
				failing = false
				s.emit(EventInfo, "connection to the server is back")
			}
		case ctx.Err() != nil:
			return
		case IsLeaseLost(err):
			s.leaseLost.Store(true)
			s.emit(EventWarning, "this PC was offline too long and someone else started hosting. Your progress from now on is saved as a separate branch the group can pick from History")
			return
		case IsUnauthorized(err):
			s.leaseLost.Store(true)
			s.emit(EventWarning, "this PC is no longer a member of the group, your progress stays on this PC only")
			return
		default:
			if !failing {
				failing = true
				s.emit(EventWarning, "cannot reach the server, still trying. Keep playing, your progress is safe on this PC")
			}
		}
	}
}

func (s *Session) prepareLocal(ctx context.Context) error {
	currentHash, fileCount, err := ManifestHash(s.folder, s.filter)
	if err != nil {
		return err
	}
	head := s.lease.BaseRevisionID
	s.parentID = head
	local := s.Config.World(s.WorldID)
	synced := local.SyncedBefore()

	if head == "" {
		if fileCount == 0 {
			s.emit(EventInfo, "the group has no save yet and no matching files are on this PC. Create the world in the game, it is uploaded when you close the game")
			return s.markUploadedState()
		}
		_, err := s.uploadCurrent(ctx, "initial world", "", false)
		return err
	}
	if synced && local.LastRevisionID == head && currentHash == local.LastManifestHash {
		s.emit(EventInfo, "this PC already has the latest save")
		return s.markUploadedState()
	}
	contentOnServer := synced && currentHash == local.LastManifestHash
	if fileCount > 0 && !contentOnServer {
		note, asBranch := "files found on this PC before its first sync", true
		s.parentID = ""
		if synced {
			note = "progress made on this PC since the last sync"
			s.parentID = local.LastRevisionID
			asBranch = s.LocalChanges != LocalChangesLatest
		}
		revision, err := s.uploadCurrent(ctx, note, "", asBranch)
		if err != nil {
			return err
		}
		if revision != nil && revision.Branch == proto.MainBranch {
			return nil
		}
		contentOnServer = revision != nil
		s.parentID = head
	}
	backup := BackupNever
	if fileCount > 0 {
		backup = BackupAlways
	}
	return s.restore(ctx, head, backup)
}

func (s *Session) markUploadedState() error {
	fingerprint, _, err := Fingerprint(s.folder, s.filter)
	s.uploadedFingerprint = fingerprint
	return err
}

func (s *Session) restore(ctx context.Context, revisionID string, backup BackupMode) error {
	result, err := RestoreRevision(ctx, s.Client, s.Config, s.world, revisionID, RestoreOptions{Backup: backup, Progress: s.progress("Downloading the latest save")})
	s.clearProgress()
	if err != nil {
		return err
	}
	if result.BackupDir != "" {
		s.emit(EventRestored, "latest save downloaded. The files that were here before are kept in %s", result.BackupDir)
	} else {
		s.emit(EventRestored, "latest save downloaded")
	}
	if len(result.Skipped) > 0 {
		s.emit(EventWarning, "%d files in the group's save are outside this PC's file filter and were not written, for example %s. If they belong to the world, widen the filter in Settings", len(result.Skipped), result.Skipped[0])
	}
	if s.headIsCheckpoint {
		s.emit(EventWarning, "the latest save is a mid-session backup, the previous host's game or PC may have stopped unexpectedly. If the world does not load, open History and make an earlier save current")
	}
	return s.markUploadedState()
}

type BackupMode int

const (
	BackupAuto BackupMode = iota
	BackupAlways
	BackupNever
)

type RestoreOptions struct {
	Backup   BackupMode
	Progress ProgressFunc
}

func gameRunningError(processName string) error {
	if processName == "" {
		return nil
	}
	if running, err := processRunning(processName); err == nil && running {
		return fmt.Errorf("%s is running. Close the game before its save is replaced, nothing was changed", normalizeProcessName(processName))
	}
	return nil
}

func RestoreRevision(ctx context.Context, client *APIClient, config *ConfigFile, world proto.World, revisionID string, options RestoreOptions) (RestoreResult, error) {
	local := config.World(world.ID)
	if !local.Confirmed {
		return RestoreResult{}, ErrNotConfirmed
	}
	folder := ExpandPath(local.SavePath)
	if err := CheckSaveFolder(folder); err != nil {
		return RestoreResult{}, err
	}
	filter := ParseInclude(local.Include)
	if err := filter.Validate(); err != nil {
		return RestoreResult{}, err
	}
	if err := gameRunningError(local.Process); err != nil {
		return RestoreResult{}, err
	}
	backup := options.Backup == BackupAlways
	if options.Backup == BackupAuto {
		_, fileCount, err := ManifestHash(folder, filter)
		if err != nil {
			return RestoreResult{}, err
		}
		backup = fileCount > 0
	}
	tempDir, err := config.TempDir()
	if err != nil {
		return RestoreResult{}, err
	}
	archivePath := filepath.Join(tempDir, fmt.Sprintf("download-%s-%d.tar.zst", revisionID, time.Now().UnixNano()))
	defer os.Remove(archivePath)
	if err := client.Download(ctx, revisionID, archivePath, options.Progress); err != nil {
		return RestoreResult{}, err
	}
	if err := gameRunningError(local.Process); err != nil {
		return RestoreResult{}, err
	}
	result, err := Restore(archivePath, folder, filter, config.BackupDir(world.ID), backup)
	if err != nil {
		return result, err
	}
	manifestHash, _, err := ManifestHash(folder, filter)
	if err != nil {
		return result, err
	}
	return result, config.UpdateWorld(world.ID, func(settings *WorldSettings) {
		settings.RecordSync(revisionID, manifestHash)
	})
}

func LocalProgressPending(config *ConfigFile, worldID string) (bool, error) {
	local := config.World(worldID)
	if !local.Confirmed || !local.SyncedBefore() {
		return false, nil
	}
	currentHash, fileCount, err := ManifestHash(ExpandPath(local.SavePath), ParseInclude(local.Include))
	if err != nil {
		return false, err
	}
	return fileCount > 0 && currentHash != local.LastManifestHash, nil
}

func (s *Session) uploadCurrent(ctx context.Context, note string, expectedFingerprint string, asBranch bool) (*proto.Revision, error) {
	tempDir, err := s.Config.TempDir()
	if err != nil {
		return nil, err
	}
	archivePath := filepath.Join(tempDir, fmt.Sprintf("upload-%s-%d.tar.zst", s.WorldID, time.Now().UnixNano()))
	defer os.Remove(archivePath)
	defer s.clearProgress()
	s.progress("Packing the save")(0, 0)
	packed, err := Pack(s.folder, s.filter, archivePath)
	if err != nil {
		return nil, err
	}
	fingerprint, _, err := Fingerprint(s.folder, s.filter)
	if err != nil {
		return nil, err
	}
	if expectedFingerprint != "" && fingerprint != expectedFingerprint {
		return nil, nil
	}
	local := s.Config.World(s.WorldID)
	if packed.FileCount == 0 || (local.SyncedBefore() && packed.ManifestHash == local.LastManifestHash && s.parentID == local.LastRevisionID) {
		s.uploadedFingerprint = fingerprint
		return nil, nil
	}
	fencingToken := s.lease.FencingToken
	if asBranch {
		fencingToken = 0
	}
	revision, err := s.Client.Upload(ctx, UploadInput{
		WorldID:      s.WorldID,
		ParentID:     s.parentID,
		FencingToken: fencingToken,
		ArchivePath:  archivePath,
		Sha256:       packed.ArchiveSha256,
		Note:         note,
	}, s.progress("Uploading "+note))
	if err != nil {
		return nil, err
	}
	s.parentID = revision.ID
	s.uploadedFingerprint = fingerprint
	if err := s.Config.UpdateWorld(s.WorldID, func(settings *WorldSettings) {
		settings.RecordSync(revision.ID, packed.ManifestHash)
	}); err != nil {
		return &revision, err
	}
	plural := "s"
	if packed.FileCount == 1 {
		plural = ""
	}
	kind, message := EventUploaded, fmt.Sprintf("%s uploaded (%d file%s)", note, packed.FileCount, plural)
	if revision.Branch != proto.MainBranch {
		kind = EventWarning
		message = fmt.Sprintf("%s: saved as the separate branch %s, not as the group's current world. Nothing is lost, the group can make it current from History", note, revision.Branch)
	} else if s.previousSize > 1<<20 && revision.Size*2 < s.previousSize {
		kind = EventWarning
		message = fmt.Sprintf("%s uploaded, but it is less than half the size of the previous save. If the wrong world was saved, make the earlier save current from History", note)
	}
	if revision.Branch == proto.MainBranch {
		s.previousSize = revision.Size
	}
	if s.OnEvent != nil {
		s.OnEvent(Event{Kind: kind, Message: message, Revision: &revision})
	}
	return &revision, nil
}

func (s *Session) startGame(ctx context.Context) (<-chan error, error) {
	done := make(chan error, 1)
	if s.settings.Process == "" {
		command, err := launchCommand(s.settings.Launch, true)
		if err != nil {
			return nil, err
		}
		if err := command.Start(); err != nil {
			return nil, err
		}
		s.emit(EventGameStarted, "game started")
		go func() {
			command.Wait()
			done <- nil
		}()
		return done, nil
	}
	processName := normalizeProcessName(s.settings.Process)
	if s.settings.Launch != "" {
		command, err := launchCommand(s.settings.Launch, false)
		if err != nil {
			return nil, err
		}
		if err := command.Start(); err != nil {
			return nil, err
		}
		go command.Wait()
		s.emit(EventInfo, "starting the game, waiting for %s", processName)
	} else {
		s.emit(EventInfo, "start the game yourself now, waiting for %s", processName)
	}
	go func() {
		sleep := func(duration time.Duration) bool {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(duration):
				return true
			}
		}
		waitingSince := time.Now()
		for {
			if running, err := processRunning(processName); err == nil && running {
				break
			}
			if time.Since(waitingSince) > s.Timings.ProcessReminder {
				waitingSince = time.Now()
				s.emit(EventWarning, "still waiting for %s. If the game is already open, its process has another name: check Task Manager and fix it in Settings. Your save is still uploaded when you press Stop hosting", processName)
			}
			if !sleep(s.Timings.ProcessPoll) {
				return
			}
		}
		s.emit(EventGameStarted, "%s is running", processName)
		missing := 0
		for missing < 2 {
			if !sleep(s.Timings.ProcessPoll) {
				return
			}
			if running, err := processRunning(processName); err == nil && !running {
				missing++
			} else {
				missing = 0
			}
		}
		done <- nil
	}()
	return done, nil
}

func (s *Session) checkpointInterval() time.Duration {
	if s.Timings.Checkpoint > 0 {
		return s.Timings.Checkpoint
	}
	return s.settings.CheckpointInterval()
}

func (s *Session) watch(ctx context.Context, gameDone <-chan error) {
	watcher := time.NewTicker(s.Timings.Watch)
	defer watcher.Stop()
	checkpointEvery := s.checkpointInterval()
	lastSeen := s.uploadedFingerprint
	lastChange := time.Now()
	lastCheckpoint := time.Now()
	for {
		select {
		case <-gameDone:
			s.emit(EventGameExited, "game closed")
			return
		case <-ctx.Done():
			s.emit(EventInfo, "stopping, saving what is on disk now")
			return
		case <-watcher.C:
		}
		if checkpointEvery == 0 || s.leaseLost.Load() {
			continue
		}
		fingerprint, _, err := Fingerprint(s.folder, s.filter)
		if err != nil {
			continue
		}
		if fingerprint != lastSeen {
			lastSeen = fingerprint
			lastChange = time.Now()
			continue
		}
		if fingerprint == s.uploadedFingerprint || time.Since(lastChange) < s.Timings.Quiet || time.Since(lastCheckpoint) < checkpointEvery {
			continue
		}
		revision, err := s.uploadCurrent(ctx, CheckpointNote, fingerprint, false)
		if err != nil && ctx.Err() == nil {
			s.emit(EventWarning, "mid-session backup failed, the next one is tried later: %v", err)
		}
		if revision != nil {
			lastCheckpoint = time.Now()
		} else {
			lastChange = time.Now()
		}
	}
}

func (s *Session) finalSync(ctx context.Context) error {
	cancelledErr := errors.New("upload cancelled. Your progress is safe on this PC and is uploaded the next time you host or sync this world")
	lastSeen, _, _ := Fingerprint(s.folder, s.filter)
	stableSince := time.Now()
	settleDeadline := time.Now().Add(2 * time.Minute)
	for time.Since(stableSince) < s.Timings.Settle && time.Now().Before(settleDeadline) {
		select {
		case <-ctx.Done():
			return cancelledErr
		case <-time.After(s.Timings.Settle / 5):
		}
		if fingerprint, _, err := Fingerprint(s.folder, s.filter); err == nil && fingerprint != lastSeen {
			lastSeen = fingerprint
			stableSince = time.Now()
		}
	}
	fingerprint, fileCount, err := Fingerprint(s.folder, s.filter)
	if err != nil {
		return err
	}
	if fileCount == 0 {
		s.emit(EventWarning, "no files in %s match the file filter, so nothing was uploaded. Open Settings and check the folder and the filter against the world's real file names", s.folder)
		return nil
	}
	if fingerprint == s.uploadedFingerprint {
		s.emit(EventInfo, "nothing changed since the last upload")
		return nil
	}
	delay := s.Timings.UploadRetry
	for {
		revision, err := s.uploadCurrent(ctx, "session end", "", false)
		if err == nil {
			if revision == nil {
				s.emit(EventInfo, "nothing changed since the last upload")
			}
			return nil
		}
		if ctx.Err() != nil {
			return cancelledErr
		}
		if IsPermanent(err) {
			return fmt.Errorf("the save could not be uploaded (%w). Your progress is safe on this PC", err)
		}
		s.emit(EventWarning, "upload failed (%v), trying again in %s. Keep this app open", err, delay.Round(time.Second))
		select {
		case <-ctx.Done():
			return cancelledErr
		case <-time.After(delay):
		}
		delay = min(delay*2, 2*time.Minute)
	}
}
