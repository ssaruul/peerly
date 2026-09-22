package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"peerly/proto"
	"peerly/server/blobs"
	"peerly/server/store"
)

const (
	maxNameLength    = 64
	maxSettingLength = 1024
	maxJoinInfo      = 200
	maxNoteLength    = 200
)

type Server struct {
	Store     *store.Store
	Blobs     *blobs.Dir
	AdminKey  string
	MaxUpload int64

	joinLimiter *rateLimiter
}

type memberHandler func(w http.ResponseWriter, r *http.Request, member proto.Member)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"service": "peerly"})
	})
	if s.joinLimiter == nil {
		s.joinLimiter = newRateLimiter(20, 20)
	}
	mux.HandleFunc("POST /groups", s.joinLimiter.wrap(s.createGroup))
	mux.HandleFunc("POST /groups/join", s.joinLimiter.wrap(s.joinGroup))
	mux.HandleFunc("POST /groups/invites", s.auth(s.createInvite))
	mux.HandleFunc("POST /groups/owner", s.auth(s.transferOwnership))
	mux.HandleFunc("GET /admin/groups", s.admin(s.listGroups))
	mux.HandleFunc("POST /admin/groups/{id}/recover", s.admin(s.recoverGroup))
	mux.HandleFunc("GET /me", s.authAny(s.me))
	mux.HandleFunc("POST /members/{id}/approve", s.auth(s.approveMember))
	mux.HandleFunc("DELETE /members/{id}", s.auth(s.revokeMember))
	mux.HandleFunc("GET /worlds", s.auth(s.listWorlds))
	mux.HandleFunc("POST /worlds", s.auth(s.createWorld))
	mux.HandleFunc("DELETE /worlds/{id}", s.auth(s.deleteWorld))
	mux.HandleFunc("POST /worlds/{id}/lease", s.auth(s.acquireLease))
	mux.HandleFunc("PUT /worlds/{id}/lease", s.auth(s.heartbeat))
	mux.HandleFunc("DELETE /worlds/{id}/lease", s.auth(s.releaseLease))
	mux.HandleFunc("GET /worlds/{id}/revisions", s.auth(s.listRevisions))
	mux.HandleFunc("POST /worlds/{id}/revisions", s.auth(s.uploadRevision))
	mux.HandleFunc("POST /worlds/{id}/promote", s.auth(s.promote))
	mux.HandleFunc("PUT /worlds/{id}/join-info", s.auth(s.setJoinInfo))
	mux.HandleFunc("GET /revisions/{id}/blob", s.auth(s.downloadBlob))
	mux.HandleFunc("DELETE /revisions/{id}", s.auth(s.discardFork))
	mux.HandleFunc("PUT /revisions/{id}/pin", s.auth(s.pinRevision))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err error) {
	var held *store.LeaseHeldError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &held):
		writeJSON(w, http.StatusConflict, proto.ErrorResponse{Error: err.Error(), Lease: &held.Lease})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrLeaseLost):
		writeJSON(w, http.StatusGone, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrGone):
		writeJSON(w, http.StatusNotFound, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrNotYours), errors.Is(err, store.ErrNotAuthor):
		writeJSON(w, http.StatusForbidden, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrPending):
		writeJSON(w, http.StatusForbidden, proto.ErrorResponse{Error: err.Error(), Pending: true})
	case errors.Is(err, store.ErrBadInvite):
		writeJSON(w, http.StatusNotFound, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrNewerDatabase):
		writeJSON(w, http.StatusServiceUnavailable, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrNameTaken):
		writeJSON(w, http.StatusConflict, proto.ErrorResponse{Error: err.Error()})
	case errors.Is(err, store.ErrNotFork), errors.Is(err, store.ErrIsHead), errors.Is(err, store.ErrSelf), errors.Is(err, store.ErrPinned), errors.Is(err, blobs.ErrChecksumMismatch):
		writeJSON(w, http.StatusUnprocessableEntity, proto.ErrorResponse{Error: err.Error()})
	case errors.As(err, &tooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, proto.ErrorResponse{Error: "save is larger than the server upload limit"})
	default:
		log.Printf("internal error: %v", err)
		writeJSON(w, http.StatusInternalServerError, proto.ErrorResponse{Error: "internal server error"})
	}
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, proto.ErrorResponse{Error: message})
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		badRequest(w, "invalid json body")
		return false
	}
	return true
}

func tooLong(w http.ResponseWriter, limit int, fields map[string]string) bool {
	for name, value := range fields {
		if utf8.RuneCountInString(value) > limit {
			badRequest(w, name+" is longer than "+strconv.Itoa(limit)+" characters")
			return true
		}
	}
	return false
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func parseToken(w http.ResponseWriter, raw string) (int64, bool) {
	if raw == "" {
		return 0, true
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		badRequest(w, "token must be an integer")
		return 0, false
	}
	return parsed, true
}

func (s *Server) auth(next memberHandler) http.HandlerFunc {
	return s.authAny(func(w http.ResponseWriter, r *http.Request, member proto.Member) {
		if member.Status != proto.MemberApproved {
			writeError(w, store.ErrPending)
			return
		}
		next(w, r, member)
	})
}

func (s *Server) authAny(next memberHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || token == "" {
			writeJSON(w, http.StatusUnauthorized, proto.ErrorResponse{Error: "missing bearer token"})
			return
		}
		member, err := s.Store.MemberByToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, proto.ErrorResponse{Error: "this device is not a member of any group on this server"})
			return
		}
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, member)
	}
}

func (s *Server) hasAdminKey(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-Admin-Key"))), []byte(s.AdminKey)) == 1
}

func (s *Server) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminKey == "" || !s.hasAdminKey(r) {
			writeJSON(w, http.StatusForbidden, proto.ErrorResponse{Error: "this needs the server admin key"})
			return
		}
		next(w, r)
	}
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Store.ListGroups(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) recoverGroup(w http.ResponseWriter, r *http.Request) {
	request := proto.RecoverRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.DisplayName == "" || tooLong(w, maxNameLength, map[string]string{"your name": request.DisplayName}) {
		if request.DisplayName == "" {
			badRequest(w, "your name is required")
		}
		return
	}
	session, err := s.Store.RecoverGroup(r.Context(), r.PathValue("id"), request.DisplayName)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("group %q recovered with the admin key, new owner %q", session.Group.Name, session.Member.DisplayName)
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) transferOwnership(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.TransferRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	if err := s.Store.TransferOwnership(r.Context(), member, request.MemberID); err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q made member %s the owner of group %s", member.DisplayName, request.MemberID, member.GroupID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request) {
	if s.AdminKey != "" && !s.hasAdminKey(r) {
		writeJSON(w, http.StatusForbidden, proto.ErrorResponse{Error: "creating groups on this server requires the admin key"})
		return
	}
	request := proto.CreateGroupRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	if request.Name == "" || request.DisplayName == "" {
		badRequest(w, "group name and your name are required")
		return
	}
	if tooLong(w, maxNameLength, map[string]string{"group name": request.Name, "your name": request.DisplayName}) {
		return
	}
	session, err := s.Store.CreateGroup(r.Context(), request.Name, request.DisplayName)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("group %q created by %q", session.Group.Name, session.Member.DisplayName)
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) joinGroup(w http.ResponseWriter, r *http.Request) {
	request := proto.JoinGroupRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	request.DeviceName = truncate(strings.TrimSpace(request.DeviceName), maxNameLength)
	if strings.TrimSpace(request.InviteCode) == "" || request.DisplayName == "" {
		badRequest(w, "invite code and your name are required")
		return
	}
	if tooLong(w, maxNameLength, map[string]string{"your name": request.DisplayName, "invite code": request.InviteCode}) {
		return
	}
	session, err := s.Store.JoinGroup(r.Context(), request.InviteCode, request.DisplayName, request.DeviceName)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q (%s) asked to join group %q", session.Member.DisplayName, session.Member.DeviceName, session.Group.Name)
	writeJSON(w, http.StatusCreated, session)
}

func (s *Server) createInvite(w http.ResponseWriter, r *http.Request, member proto.Member) {
	invite, err := s.Store.CreateInvite(r.Context(), member)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q created an invite for group %s", member.DisplayName, member.GroupID)
	writeJSON(w, http.StatusCreated, invite)
}

func (s *Server) approveMember(w http.ResponseWriter, r *http.Request, member proto.Member) {
	if err := s.Store.ApproveMember(r.Context(), member, r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q approved member %s in group %s", member.DisplayName, r.PathValue("id"), member.GroupID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeMember(w http.ResponseWriter, r *http.Request, member proto.Member) {
	if err := s.Store.RevokeMember(r.Context(), member, r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q removed member %s from group %s", member.DisplayName, r.PathValue("id"), member.GroupID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request, member proto.Member) {
	response, err := s.Store.Me(r.Context(), member)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) listWorlds(w http.ResponseWriter, r *http.Request, member proto.Member) {
	statuses, err := s.Store.ListWorlds(r.Context(), member)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, statuses)
}

func (s *Server) createWorld(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.CreateWorldRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.GameName = strings.TrimSpace(request.GameName)
	if request.Name == "" {
		badRequest(w, "world name is required")
		return
	}
	if tooLong(w, maxNameLength, map[string]string{"world name": request.Name, "game name": request.GameName}) {
		return
	}
	if tooLong(w, maxSettingLength, map[string]string{
		"save folder": request.DefaultSavePath, "launch command": request.DefaultLaunch,
		"process name": request.DefaultProcess, "file filter": request.DefaultInclude,
	}) {
		return
	}
	world, err := s.Store.CreateWorld(r.Context(), member, request)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q added world %q (%s)", member.DisplayName, world.Name, world.ID)
	writeJSON(w, http.StatusCreated, world)
}

func (s *Server) deleteWorld(w http.ResponseWriter, r *http.Request, member proto.Member) {
	blobIDs, err := s.Store.DeleteWorld(r.Context(), member, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	for _, blobID := range blobIDs {
		s.Blobs.Remove(blobID)
	}
	log.Printf("%q deleted world %s with %d saves", member.DisplayName, r.PathValue("id"), len(blobIDs))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) acquireLease(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.AcquireRequest{}
	if r.ContentLength != 0 && !readJSON(w, r, &request) {
		return
	}
	lease, err := s.Store.AcquireLease(r.Context(), member, r.PathValue("id"), truncate(request.SessionID, maxNameLength))
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q holds the lease of world %s (token %d)", member.DisplayName, lease.WorldID, lease.FencingToken)
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.LeaseRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	lease, err := s.Store.Heartbeat(r.Context(), member, r.PathValue("id"), request.FencingToken)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, lease)
}

func (s *Server) releaseLease(w http.ResponseWriter, r *http.Request, member proto.Member) {
	fencingToken, valid := parseToken(w, r.URL.Query().Get("token"))
	if !valid {
		return
	}
	if err := s.Store.ReleaseLease(r.Context(), member, r.PathValue("id"), fencingToken); err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q released the lease of world %s", member.DisplayName, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) setJoinInfo(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.JoinInfoRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	joinInfo := truncate(strings.TrimSpace(request.JoinInfo), maxJoinInfo)
	if err := s.Store.SetJoinInfo(r.Context(), member, r.PathValue("id"), request.FencingToken, joinInfo); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request, member proto.Member) {
	revisions, err := s.Store.ListRevisions(r.Context(), member, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, revisions)
}

func (s *Server) uploadRevision(w http.ResponseWriter, r *http.Request, member proto.Member) {
	worldID := r.PathValue("id")
	if _, err := s.Store.World(r.Context(), member, worldID); err != nil {
		writeError(w, err)
		return
	}
	query := r.URL.Query()
	expectedSha256 := strings.ToLower(query.Get("sha256"))
	if len(expectedSha256) != 64 {
		badRequest(w, "sha256 query parameter is required")
		return
	}
	fencingToken, valid := parseToken(w, query.Get("token"))
	if !valid {
		return
	}
	if r.ContentLength > s.MaxUpload {
		writeJSON(w, http.StatusRequestEntityTooLarge, proto.ErrorResponse{Error: "save is larger than the server upload limit"})
		return
	}
	blobID := store.NewID()
	body := &idleDeadlineReader{reader: http.MaxBytesReader(w, r.Body, s.MaxUpload), controller: http.NewResponseController(w)}
	size, err := s.Blobs.Store(blobID, expectedSha256, body)
	http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		writeError(w, err)
		return
	}
	revision, orphanedBlobs, err := s.Store.Commit(r.Context(), member, store.CommitInput{
		WorldID:      worldID,
		ParentID:     query.Get("parent"),
		FencingToken: fencingToken,
		BlobID:       blobID,
		Sha256:       expectedSha256,
		Size:         size,
		Note:         truncate(query.Get("note"), maxNoteLength),
	})
	if err != nil {
		s.Blobs.Remove(blobID)
		writeError(w, err)
		return
	}
	s.removeBlobs(orphanedBlobs)
	log.Printf("%q uploaded %d bytes to world %s on %s (%s)", member.DisplayName, size, worldID, revision.Branch, revision.Note)
	writeJSON(w, http.StatusCreated, revision)
}

const uploadIdleTimeout = 2 * time.Minute

type idleDeadlineReader struct {
	reader     io.Reader
	controller *http.ResponseController
}

func (r *idleDeadlineReader) Read(buffer []byte) (int, error) {
	r.controller.SetReadDeadline(time.Now().Add(uploadIdleTimeout))
	return r.reader.Read(buffer)
}

func (s *Server) removeBlobs(blobIDs []string) {
	for _, blobID := range blobIDs {
		if err := s.Blobs.Remove(blobID); err != nil {
			log.Printf("could not remove save file %s: %v", blobID, err)
		}
	}
}

func (s *Server) promote(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.PromoteRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	revision, orphanedBlobs, err := s.Store.Promote(r.Context(), member, r.PathValue("id"), request.RevisionID)
	if err != nil {
		writeError(w, err)
		return
	}
	s.removeBlobs(orphanedBlobs)
	log.Printf("%q made revision %s the current save of world %s", member.DisplayName, request.RevisionID, r.PathValue("id"))
	writeJSON(w, http.StatusCreated, revision)
}

func (s *Server) downloadBlob(w http.ResponseWriter, r *http.Request, member proto.Member) {
	revision, blobID, err := s.Store.RevisionBlob(r.Context(), member, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Sha256", revision.Sha256)
	http.ServeFile(w, r, s.Blobs.Path(blobID))
}

func (s *Server) pinRevision(w http.ResponseWriter, r *http.Request, member proto.Member) {
	request := proto.PinRequest{}
	if !readJSON(w, r, &request) {
		return
	}
	revision, err := s.Store.PinRevision(r.Context(), member, r.PathValue("id"), request.Pinned)
	if err != nil {
		writeError(w, err)
		return
	}
	log.Printf("%q set kept=%v on revision %s", member.DisplayName, request.Pinned, revision.ID)
	writeJSON(w, http.StatusOK, revision)
}

func (s *Server) discardFork(w http.ResponseWriter, r *http.Request, member proto.Member) {
	orphanedBlobID, err := s.Store.DiscardFork(r.Context(), member, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	if orphanedBlobID != "" {
		s.removeBlobs([]string{orphanedBlobID})
	}
	log.Printf("%q deleted branch save %s", member.DisplayName, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
