package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"peerly/proto"
)

var ErrDownloadCorrupt = errors.New("downloaded save does not match the server checksum")

type APIError struct {
	Status  int
	Message string
	Lease   *proto.Lease
	Pending bool
}

func (e *APIError) Error() string {
	return e.Message
}

func IsLeaseHeld(err error) (*proto.Lease, bool) {
	var apiError *APIError
	if errors.As(err, &apiError) && apiError.Status == http.StatusConflict && apiError.Lease != nil {
		return apiError.Lease, true
	}
	return nil, false
}

func IsLeaseLost(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.Status == http.StatusGone
}

func IsUnauthorized(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.Status == http.StatusUnauthorized
}

func IsPending(err error) bool {
	var apiError *APIError
	return errors.As(err, &apiError) && apiError.Pending
}

func IsPermanent(err error) bool {
	var apiError *APIError
	if !errors.As(err, &apiError) {
		return false
	}
	switch apiError.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

type ProgressFunc func(done int64, total int64)

type progressReader struct {
	reader   io.Reader
	done     int64
	total    int64
	report   ProgressFunc
	lastSent time.Time
}

func (p *progressReader) Read(buffer []byte) (int, error) {
	count, err := p.reader.Read(buffer)
	p.done += int64(count)
	if p.report != nil && (time.Since(p.lastSent) > 400*time.Millisecond || err != nil) {
		p.lastSent = time.Now()
		p.report(p.done, p.total)
	}
	return count, err
}

type APIClient struct {
	BaseURL  string
	Token    string
	AdminKey string
	HTTP     *http.Client
}

var sharedHTTP = &http.Client{Transport: &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   15 * time.Second,
	ResponseHeaderTimeout: 5 * time.Minute,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConnsPerHost:   4,
	ForceAttemptHTTP2:     true,
}}

func NewAPIClient(baseURL string, token string) *APIClient {
	return &APIClient{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: sharedHTTP}
}

func (c *APIClient) ResolveBaseURL(ctx context.Context) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
	if err != nil {
		return "", err
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	answer := map[string]string{}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<16)).Decode(&answer) != nil || answer["service"] != "peerly" {
		return "", errors.New("something answers at this address but it is not a peerly server")
	}
	final := *response.Request.URL
	final.Path = strings.TrimSuffix(final.Path, "/health")
	final.RawQuery = ""
	return strings.TrimRight(final.String(), "/"), nil
}

func (c *APIClient) send(ctx context.Context, method string, path string, body io.Reader, contentLength int64, getBody func() (io.ReadCloser, error)) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentLength > 0 {
		request.ContentLength = contentLength
	}
	if getBody != nil {
		request.GetBody = getBody
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.AdminKey != "" {
		request.Header.Set("X-Admin-Key", c.AdminKey)
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		defer response.Body.Close()
		decoded := proto.ErrorResponse{}
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if json.Unmarshal(raw, &decoded) != nil || decoded.Error == "" {
			decoded.Error = fmt.Sprintf("the server answered %s, check the server address", response.Status)
		}
		return nil, &APIError{Status: response.StatusCode, Message: decoded.Error, Lease: decoded.Lease, Pending: decoded.Pending}
	}
	return response, nil
}

func (c *APIClient) call(ctx context.Context, method string, path string, requestBody any, responseBody any) error {
	var reader io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	response, err := c.send(ctx, method, path, reader, 0, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if responseBody == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(responseBody)
}

func (c *APIClient) CreateGroup(ctx context.Context, name string, displayName string) (proto.SessionResponse, error) {
	session := proto.SessionResponse{}
	err := c.call(ctx, http.MethodPost, "/groups", proto.CreateGroupRequest{Name: name, DisplayName: displayName}, &session)
	return session, err
}

func (c *APIClient) JoinGroup(ctx context.Context, inviteCode string, displayName string, deviceName string) (proto.SessionResponse, error) {
	session := proto.SessionResponse{}
	err := c.call(ctx, http.MethodPost, "/groups/join", proto.JoinGroupRequest{InviteCode: inviteCode, DisplayName: displayName, DeviceName: deviceName}, &session)
	return session, err
}

func (c *APIClient) Me(ctx context.Context) (proto.MeResponse, error) {
	me := proto.MeResponse{}
	return me, c.call(ctx, http.MethodGet, "/me", nil, &me)
}

func (c *APIClient) Worlds(ctx context.Context) ([]proto.WorldStatus, error) {
	statuses := []proto.WorldStatus{}
	return statuses, c.call(ctx, http.MethodGet, "/worlds", nil, &statuses)
}

func (c *APIClient) WorldStatus(ctx context.Context, worldID string) (proto.WorldStatus, error) {
	statuses, err := c.Worlds(ctx)
	if err != nil {
		return proto.WorldStatus{}, err
	}
	for _, status := range statuses {
		if status.World.ID == worldID || strings.EqualFold(status.World.Name, worldID) {
			return status, nil
		}
	}
	return proto.WorldStatus{}, fmt.Errorf("no world named %q in this group", worldID)
}

func (c *APIClient) CreateWorld(ctx context.Context, request proto.CreateWorldRequest) (proto.World, error) {
	world := proto.World{}
	return world, c.call(ctx, http.MethodPost, "/worlds", request, &world)
}

func (c *APIClient) AcquireLease(ctx context.Context, worldID string, sessionID string) (proto.Lease, error) {
	lease := proto.Lease{}
	return lease, c.call(ctx, http.MethodPost, "/worlds/"+worldID+"/lease", proto.AcquireRequest{SessionID: sessionID}, &lease)
}

func (c *APIClient) TransferOwnership(ctx context.Context, memberID string) error {
	return c.call(ctx, http.MethodPost, "/groups/owner", proto.TransferRequest{MemberID: memberID}, nil)
}

func (c *APIClient) AdminGroups(ctx context.Context) ([]proto.AdminGroup, error) {
	groups := []proto.AdminGroup{}
	return groups, c.call(ctx, http.MethodGet, "/admin/groups", nil, &groups)
}

func (c *APIClient) RecoverGroup(ctx context.Context, groupID string, displayName string) (proto.SessionResponse, error) {
	session := proto.SessionResponse{}
	return session, c.call(ctx, http.MethodPost, "/admin/groups/"+groupID+"/recover", proto.RecoverRequest{DisplayName: displayName}, &session)
}

func (c *APIClient) Heartbeat(ctx context.Context, worldID string, fencingToken int64) error {
	return c.call(ctx, http.MethodPut, "/worlds/"+worldID+"/lease", proto.LeaseRequest{FencingToken: fencingToken}, nil)
}

func (c *APIClient) ReleaseLease(ctx context.Context, worldID string, fencingToken int64) error {
	return c.call(ctx, http.MethodDelete, "/worlds/"+worldID+"/lease?token="+strconv.FormatInt(fencingToken, 10), nil, nil)
}

func (c *APIClient) SetJoinInfo(ctx context.Context, worldID string, fencingToken int64, joinInfo string) error {
	return c.call(ctx, http.MethodPut, "/worlds/"+worldID+"/join-info", proto.JoinInfoRequest{FencingToken: fencingToken, JoinInfo: joinInfo}, nil)
}

func (c *APIClient) Revisions(ctx context.Context, worldID string) ([]proto.Revision, error) {
	revisions := []proto.Revision{}
	return revisions, c.call(ctx, http.MethodGet, "/worlds/"+worldID+"/revisions", nil, &revisions)
}

func (c *APIClient) Promote(ctx context.Context, worldID string, revisionID string) (proto.Revision, error) {
	revision := proto.Revision{}
	return revision, c.call(ctx, http.MethodPost, "/worlds/"+worldID+"/promote", proto.PromoteRequest{RevisionID: revisionID}, &revision)
}

func (c *APIClient) DiscardFork(ctx context.Context, revisionID string) error {
	return c.call(ctx, http.MethodDelete, "/revisions/"+revisionID, nil, nil)
}

func (c *APIClient) PinRevision(ctx context.Context, revisionID string, pinned bool) (proto.Revision, error) {
	revision := proto.Revision{}
	return revision, c.call(ctx, http.MethodPut, "/revisions/"+revisionID+"/pin", proto.PinRequest{Pinned: pinned}, &revision)
}

func (c *APIClient) UpdateWorld(ctx context.Context, worldID string, request proto.UpdateWorldRequest) (proto.World, error) {
	world := proto.World{}
	return world, c.call(ctx, http.MethodPatch, "/worlds/"+worldID, request, &world)
}

func (c *APIClient) DeleteWorld(ctx context.Context, worldID string) error {
	return c.call(ctx, http.MethodDelete, "/worlds/"+worldID, nil, nil)
}

func (c *APIClient) CreateInvite(ctx context.Context) (proto.Invite, error) {
	invite := proto.Invite{}
	return invite, c.call(ctx, http.MethodPost, "/groups/invites", nil, &invite)
}

func (c *APIClient) ApproveMember(ctx context.Context, memberID string) error {
	return c.call(ctx, http.MethodPost, "/members/"+memberID+"/approve", nil, nil)
}

func (c *APIClient) RemoveMember(ctx context.Context, memberID string) error {
	return c.call(ctx, http.MethodDelete, "/members/"+memberID, nil, nil)
}

func (c *APIClient) Health(ctx context.Context) error {
	response := map[string]string{}
	if err := c.call(ctx, http.MethodGet, "/health", nil, &response); err != nil {
		return err
	}
	if response["service"] != "peerly" {
		return errors.New("something answers at this address but it is not a peerly server")
	}
	return nil
}

type UploadInput struct {
	WorldID      string
	ParentID     string
	FencingToken int64
	ArchivePath  string
	Sha256       string
	Note         string
}

func (c *APIClient) Upload(ctx context.Context, input UploadInput, progress ProgressFunc) (proto.Revision, error) {
	revision := proto.Revision{}
	archive, err := os.Open(input.ArchivePath)
	if err != nil {
		return revision, err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return revision, err
	}
	query := url.Values{}
	query.Set("parent", input.ParentID)
	query.Set("token", strconv.FormatInt(input.FencingToken, 10))
	query.Set("sha256", input.Sha256)
	query.Set("note", input.Note)
	body := io.NopCloser(&progressReader{reader: archive, total: info.Size(), report: progress})
	reopen := func() (io.ReadCloser, error) {
		again, err := os.Open(input.ArchivePath)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{&progressReader{reader: again, total: info.Size(), report: progress}, again}, nil
	}
	response, err := c.send(ctx, http.MethodPost, "/worlds/"+input.WorldID+"/revisions?"+query.Encode(), body, info.Size(), reopen)
	if err != nil {
		return revision, err
	}
	defer response.Body.Close()
	return revision, json.NewDecoder(response.Body).Decode(&revision)
}

func (c *APIClient) Download(ctx context.Context, revisionID string, destination string, progress ProgressFunc) error {
	response, err := c.send(ctx, http.MethodGet, "/revisions/"+revisionID+"/blob", nil, 0, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	source := &progressReader{reader: response.Body, total: response.ContentLength, report: progress}
	_, copyErr := io.Copy(io.MultiWriter(output, hasher), source)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		os.Remove(destination)
		return err
	}
	if expected := response.Header.Get("X-Sha256"); expected != "" && expected != hex.EncodeToString(hasher.Sum(nil)) {
		os.Remove(destination)
		return ErrDownloadCorrupt
	}
	return nil
}
