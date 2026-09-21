package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"peerly/proto"
	"peerly/server/blobs"
	"peerly/server/store"
)

type testClient struct {
	t       *testing.T
	baseURL string
	token   string
}

func (c *testClient) do(method string, path string, body any, headers map[string]string) (int, []byte) {
	c.t.Helper()
	var reader io.Reader
	switch typed := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(typed)
	default:
		encoded, _ := json.Marshal(typed)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, payload
}

func decode[T any](t *testing.T, payload []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return value
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestHostHandoffOverHTTP(t *testing.T) {
	dataDir := t.TempDir()
	database, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	blobDir, err := blobs.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&Server{Store: database, Blobs: blobDir, AdminKey: "secret", MaxUpload: 1 << 20}).Handler())
	defer server.Close()

	anonymous := &testClient{t: t, baseURL: server.URL}
	groupRequest := proto.CreateGroupRequest{Name: "friends", DisplayName: "a"}
	if status, _ := anonymous.do("POST", "/groups", groupRequest, nil); status != http.StatusForbidden {
		t.Fatalf("group creation without admin key = %d", status)
	}
	status, payload := anonymous.do("POST", "/groups", groupRequest, map[string]string{"X-Admin-Key": "secret"})
	if status != http.StatusCreated {
		t.Fatalf("create group = %d %s", status, payload)
	}
	sessionA := decode[proto.SessionResponse](t, payload)
	_, payload = anonymous.do("POST", "/groups/join", proto.JoinGroupRequest{InviteCode: sessionA.Group.InviteCode, DisplayName: "b"}, nil)
	sessionB := decode[proto.SessionResponse](t, payload)
	clientA := &testClient{t: t, baseURL: server.URL, token: sessionA.Token}
	clientB := &testClient{t: t, baseURL: server.URL, token: sessionB.Token}

	if status, _ := anonymous.do("GET", "/worlds", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d", status)
	}
	_, payload = clientA.do("POST", "/worlds", proto.CreateWorldRequest{Name: "base", GameName: "valheim"}, nil)
	world := decode[proto.World](t, payload)

	_, payload = clientA.do("POST", "/worlds/"+world.ID+"/lease", nil, nil)
	leaseA := decode[proto.Lease](t, payload)
	status, payload = clientB.do("POST", "/worlds/"+world.ID+"/lease", nil, nil)
	refusal := decode[proto.ErrorResponse](t, payload)
	if status != http.StatusConflict || refusal.Lease == nil || refusal.Lease.HolderName != "a" || refusal.Lease.FencingToken != 0 {
		t.Fatalf("b acquire while a hosts = %d %s", status, payload)
	}
	clientA.do("PUT", "/worlds/"+world.ID+"/join-info", proto.JoinInfoRequest{FencingToken: leaseA.FencingToken, JoinInfo: "code 1234"}, nil)
	_, payload = clientB.do("GET", "/worlds", nil, nil)
	statuses := decode[[]proto.WorldStatus](t, payload)
	if len(statuses) != 1 || statuses[0].World.JoinInfo != "code 1234" || statuses[0].Lease == nil || statuses[0].Lease.FencingToken != 0 {
		t.Fatalf("world list seen by b = %s", payload)
	}

	saveA := []byte("world state after a's session")
	uploadPath := fmt.Sprintf("/worlds/%s/revisions?parent=%s&token=%d&sha256=", world.ID, leaseA.BaseRevisionID, leaseA.FencingToken)
	if status, _ := clientA.do("POST", uploadPath+checksum([]byte("different")), saveA, nil); status != http.StatusUnprocessableEntity {
		t.Fatalf("checksum mismatch upload = %d", status)
	}
	status, payload = clientA.do("POST", uploadPath+checksum(saveA), saveA, nil)
	revisionA := decode[proto.Revision](t, payload)
	if status != http.StatusCreated || revisionA.Branch != proto.MainBranch {
		t.Fatalf("a upload = %d %s", status, payload)
	}
	if status, _ := clientA.do("DELETE", fmt.Sprintf("/worlds/%s/lease?token=%d", world.ID, leaseA.FencingToken), nil, nil); status != http.StatusNoContent {
		t.Fatalf("release = %d", status)
	}

	status, payload = clientB.do("POST", "/worlds/"+world.ID+"/lease", nil, nil)
	leaseB := decode[proto.Lease](t, payload)
	if status != http.StatusOK || leaseB.BaseRevisionID != revisionA.ID {
		t.Fatalf("b acquire after release = %d %s", status, payload)
	}
	status, payload = clientB.do("GET", "/revisions/"+leaseB.BaseRevisionID+"/blob", nil, nil)
	if status != http.StatusOK || !bytes.Equal(payload, saveA) {
		t.Fatalf("b download = %d %q", status, payload)
	}

	staleSave := []byte("a kept playing offline")
	stalePath := fmt.Sprintf("/worlds/%s/revisions?parent=%s&token=%d&sha256=%s", world.ID, revisionA.ID, leaseA.FencingToken, checksum(staleSave))
	_, payload = clientA.do("POST", stalePath, staleSave, nil)
	fork := decode[proto.Revision](t, payload)
	if fork.Branch == proto.MainBranch {
		t.Fatal("stale upload landed on main")
	}
	if status, _ := clientA.do("POST", "/worlds/"+world.ID+"/promote", proto.PromoteRequest{RevisionID: fork.ID}, nil); status != http.StatusConflict {
		t.Fatalf("promote while b hosts = %d", status)
	}

	longName := proto.CreateWorldRequest{Name: string(bytes.Repeat([]byte("x"), 65))}
	if status, _ := clientA.do("POST", "/worlds", longName, nil); status != http.StatusBadRequest {
		t.Fatalf("overlong world name = %d", status)
	}
	if status, _ := clientB.do("POST", "/groups/invite", nil, nil); status != http.StatusForbidden {
		t.Fatalf("non-owner invite rotation = %d", status)
	}
	if status, _ := clientB.do("DELETE", "/members/"+sessionA.Member.ID, nil, nil); status != http.StatusForbidden {
		t.Fatalf("non-owner member removal = %d", status)
	}

	outsiderStatus, outsiderPayload := anonymous.do("POST", "/groups", proto.CreateGroupRequest{Name: "strangers", DisplayName: "x"}, map[string]string{"X-Admin-Key": "secret"})
	if outsiderStatus != http.StatusCreated {
		t.Fatal("outsider group creation failed")
	}
	outsider := &testClient{t: t, baseURL: server.URL, token: decode[proto.SessionResponse](t, outsiderPayload).Token}
	if status, _ := outsider.do("GET", "/revisions/"+revisionA.ID+"/blob", nil, nil); status != http.StatusNotFound {
		t.Fatalf("outsider download = %d", status)
	}
	for _, probe := range [][2]string{
		{"POST", "/worlds/" + world.ID + "/lease"},
		{"GET", "/worlds/" + world.ID + "/revisions"},
		{"DELETE", "/worlds/" + world.ID},
		{"DELETE", "/revisions/" + fork.ID},
		{"DELETE", "/members/" + sessionB.Member.ID},
	} {
		if status, _ := outsider.do(probe[0], probe[1], nil, nil); status != http.StatusNotFound && status != http.StatusForbidden {
			t.Fatalf("outsider %s %s = %d", probe[0], probe[1], status)
		}
	}
	if status, _ := outsider.do("POST", "/worlds/"+world.ID+"/promote", proto.PromoteRequest{RevisionID: fork.ID}, nil); status != http.StatusNotFound {
		t.Fatalf("outsider promote = %d", status)
	}

	if status, _ := clientA.do("DELETE", "/members/"+sessionB.Member.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("owner removes b = %d", status)
	}
	if status, _ := clientB.do("GET", "/worlds", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("removed member still has access = %d", status)
	}
}
