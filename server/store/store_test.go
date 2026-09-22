package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"peerly/proto"
)

type fixture struct {
	store      *Store
	clock      time.Time
	world      proto.World
	members    []proto.Member
	shaCounter int
}

func newFixture(t *testing.T, memberCount int) *fixture {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	f := &fixture{store: database, clock: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)}
	database.Now = func() time.Time { return f.clock }
	ctx := context.Background()
	session, err := database.CreateGroup(ctx, "friends", "a")
	if err != nil {
		t.Fatal(err)
	}
	f.members = append(f.members, session.Member)
	for index := 1; index < memberCount; index++ {
		invite, err := database.CreateInvite(ctx, session.Member)
		if err != nil {
			t.Fatal(err)
		}
		joined, err := database.JoinGroup(ctx, invite.Code, string(rune('a'+index)), "PC-"+string(rune('a'+index)))
		if err != nil {
			t.Fatal(err)
		}
		if err := database.ApproveMember(ctx, session.Member, joined.Member.ID); err != nil {
			t.Fatal(err)
		}
		joined.Member.Status = proto.MemberApproved
		f.members = append(f.members, joined.Member)
	}
	f.world, err = database.CreateWorld(ctx, session.Member, proto.CreateWorldRequest{Name: "base", GameName: "valheim"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) commit(t *testing.T, member proto.Member, parentID string, fencingToken int64) proto.Revision {
	t.Helper()
	return f.commitTo(t, f.world.ID, member, parentID, fencingToken)
}

func (f *fixture) commitTo(t *testing.T, worldID string, member proto.Member, parentID string, fencingToken int64) proto.Revision {
	t.Helper()
	f.shaCounter++
	return f.commitSha(t, worldID, member, parentID, fencingToken, fmt.Sprintf("sha-%d", f.shaCounter))
}

func (f *fixture) commitSha(t *testing.T, worldID string, member proto.Member, parentID string, fencingToken int64, sha string) proto.Revision {
	t.Helper()
	revision, _, err := f.store.Commit(context.Background(), member, CommitInput{
		WorldID: worldID, ParentID: parentID, FencingToken: fencingToken, BlobID: NewID(), Sha256: sha, Size: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Second)
	return revision
}

func (f *fixture) head(t *testing.T) string {
	t.Helper()
	world, err := f.store.World(context.Background(), f.members[0], f.world.ID)
	if err != nil {
		t.Fatal(err)
	}
	return world.HeadRevisionID
}

func TestConcurrentAcquireHasOneWinner(t *testing.T) {
	f := newFixture(t, 8)
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	winners := 0
	refused := 0
	for _, member := range f.members {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, err := f.store.AcquireLease(context.Background(), member, f.world.ID, "s")
			var held *LeaseHeldError
			mutex.Lock()
			defer mutex.Unlock()
			if err == nil {
				winners++
			} else if errors.As(err, &held) {
				refused++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	waitGroup.Wait()
	if winners != 1 || refused != len(f.members)-1 {
		t.Fatalf("winners=%d refused=%d", winners, refused)
	}
}

func TestExpiredLeaseIsReclaimableAndTokenIncreases(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	first, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s"); err == nil {
		t.Fatal("second member acquired a live lease")
	}
	f.clock = f.clock.Add(f.store.LeaseTTL + time.Second)
	second, err := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if second.FencingToken <= first.FencingToken {
		t.Fatalf("token did not increase: %d then %d", first.FencingToken, second.FencingToken)
	}
	if _, err := f.store.Heartbeat(ctx, f.members[0], f.world.ID, first.FencingToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale heartbeat error = %v", err)
	}
}

func TestHeartbeatKeepsLeaseAlive(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	for range 5 {
		f.clock = f.clock.Add(f.store.LeaseTTL - time.Second)
		if _, err := f.store.Heartbeat(ctx, f.members[0], f.world.ID, lease.FencingToken); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s"); err == nil {
		t.Fatal("lease was taken despite heartbeats")
	}
}

func TestHandoffAdvancesMain(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	leaseA, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	revisionA := f.commit(t, f.members[0], leaseA.BaseRevisionID, leaseA.FencingToken)
	if revisionA.Branch != proto.MainBranch {
		t.Fatalf("first commit branch = %s", revisionA.Branch)
	}
	checkpoint := f.commit(t, f.members[0], revisionA.ID, leaseA.FencingToken)
	if checkpoint.Branch != proto.MainBranch {
		t.Fatalf("checkpoint branch = %s", checkpoint.Branch)
	}
	f.store.ReleaseLease(ctx, f.members[0], f.world.ID, leaseA.FencingToken)
	leaseB, err := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if leaseB.BaseRevisionID != checkpoint.ID {
		t.Fatalf("b base = %s want %s", leaseB.BaseRevisionID, checkpoint.ID)
	}
	revisionB := f.commit(t, f.members[1], leaseB.BaseRevisionID, leaseB.FencingToken)
	if revisionB.Branch != proto.MainBranch || f.head(t) != revisionB.ID {
		t.Fatalf("b commit branch = %s head = %s", revisionB.Branch, f.head(t))
	}
}

func TestStaleHostBecomesFork(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	leaseA, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	base := f.commit(t, f.members[0], "", leaseA.FencingToken)
	f.clock = f.clock.Add(f.store.LeaseTTL + time.Second)
	leaseB, _ := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s")
	revisionB := f.commit(t, f.members[1], base.ID, leaseB.FencingToken)
	late := f.commit(t, f.members[0], base.ID, leaseA.FencingToken)
	if !strings.HasPrefix(late.Branch, "fork/a/") {
		t.Fatalf("stale upload branch = %s", late.Branch)
	}
	if f.head(t) != revisionB.ID {
		t.Fatal("stale upload moved head")
	}
	followUp := f.commit(t, f.members[0], late.ID, 0)
	if followUp.Branch != late.Branch {
		t.Fatalf("follow-up branch = %s want %s", followUp.Branch, late.Branch)
	}
}

func TestUploadWithoutLeaseBecomesFork(t *testing.T) {
	f := newFixture(t, 2)
	offline := f.commit(t, f.members[1], "", 0)
	if offline.Branch == proto.MainBranch || f.head(t) != "" {
		t.Fatalf("leaseless upload branch = %s head = %q", offline.Branch, f.head(t))
	}
}

func TestReacquireAfterExpiryWithUnchangedHeadStaysOnMain(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	leaseA, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	base := f.commit(t, f.members[0], "", leaseA.FencingToken)
	f.clock = f.clock.Add(f.store.LeaseTTL + time.Minute)
	if _, err := f.store.Heartbeat(ctx, f.members[0], f.world.ID, leaseA.FencingToken); err != nil {
		t.Fatalf("heartbeat after silent expiry with no other host: %v", err)
	}
	revision := f.commit(t, f.members[0], base.ID, leaseA.FencingToken)
	if revision.Branch != proto.MainBranch {
		t.Fatalf("branch = %s", revision.Branch)
	}
}

func TestPromotePreservesHistoryAndRespectsLease(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	leaseA, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	mainRevision := f.commit(t, f.members[0], "", leaseA.FencingToken)
	fork := f.commit(t, f.members[1], "", 0)
	var held *LeaseHeldError
	if _, _, err := f.store.Promote(ctx, f.members[1], f.world.ID, fork.ID); !errors.As(err, &held) {
		t.Fatalf("promote during live lease error = %v", err)
	}
	f.store.ReleaseLease(ctx, f.members[0], f.world.ID, leaseA.FencingToken)
	promoted, _, err := f.store.Promote(ctx, f.members[1], f.world.ID, fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.ParentID != mainRevision.ID || promoted.Branch != proto.MainBranch || f.head(t) != promoted.ID {
		t.Fatalf("promoted = %+v head = %s", promoted, f.head(t))
	}
	revisions, _ := f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	if len(revisions) != 3 {
		t.Fatalf("history length = %d", len(revisions))
	}
	orphanedBlob, err := f.store.DiscardFork(ctx, f.members[1], fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if orphanedBlob != "" {
		t.Fatal("blob reported orphaned while the promoted revision still uses it")
	}
	if _, err := f.store.DiscardFork(ctx, f.members[1], mainRevision.ID); !errors.Is(err, ErrNotFork) {
		t.Fatalf("discarding main revision error = %v", err)
	}
}

func TestOtherGroupCannotSeeWorld(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	outsider, err := f.store.CreateGroup(ctx, "strangers", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AcquireLease(ctx, outsider.Member, f.world.ID, "s"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider acquire error = %v", err)
	}
}

func TestRetentionKeepsNewestMainRevisionsAndForks(t *testing.T) {
	f := newFixture(t, 2)
	f.store.KeepMain = 3
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	fork := f.commit(t, f.members[1], "", 0)
	parentID := ""
	orphanedTotal := 0
	for range 6 {
		revision, orphanedBlobs, err := f.store.Commit(ctx, f.members[0], CommitInput{
			WorldID: f.world.ID, ParentID: parentID, FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: "00", Size: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(time.Second)
		parentID = revision.ID
		orphanedTotal += len(orphanedBlobs)
	}
	revisions, _ := f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	mainCount, forkKept := 0, false
	for _, revision := range revisions {
		if revision.Branch == proto.MainBranch {
			mainCount++
		}
		if revision.ID == fork.ID {
			forkKept = true
		}
	}
	if mainCount != 3 || !forkKept || orphanedTotal != 3 || f.head(t) != parentID {
		t.Fatalf("main=%d forkKept=%v orphaned=%d", mainCount, forkKept, orphanedTotal)
	}
}

func TestSameHolderReacquireReturnsCurrentHead(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	first := f.commit(t, f.members[0], "", lease.FencingToken)
	checkpoint := f.commit(t, f.members[0], first.ID, lease.FencingToken)
	again, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	if err != nil {
		t.Fatal(err)
	}
	if again.FencingToken != lease.FencingToken || again.BaseRevisionID != checkpoint.ID {
		t.Fatalf("restarted client got token %d base %s, want token %d base %s", again.FencingToken, again.BaseRevisionID, lease.FencingToken, checkpoint.ID)
	}
}

func TestForkRetentionNeverTouchesMain(t *testing.T) {
	f := newFixture(t, 2)
	f.store.KeepForks = 2
	f.store.ForkGrace = 0
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	mainRevision := f.commit(t, f.members[0], "", lease.FencingToken)
	for index := range 5 {
		f.commitSha(t, f.world.ID, f.members[1], "", 0, fmt.Sprintf("fork-%d", index))
	}
	revisions, _ := f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	forks := 0
	for _, revision := range revisions {
		if revision.Branch != proto.MainBranch {
			forks++
		}
	}
	if forks != 2 || f.head(t) != mainRevision.ID {
		t.Fatalf("forks=%d head=%s", forks, f.head(t))
	}
}

func TestPruneKeepsHeadEvenWhenClockJumpsBack(t *testing.T) {
	f := newFixture(t, 1)
	f.store.KeepMain = 2
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	parentID := ""
	for range 3 {
		parentID = f.commit(t, f.members[0], parentID, lease.FencingToken).ID
	}
	f.clock = f.clock.Add(-24 * time.Hour)
	latest := f.commit(t, f.members[0], parentID, lease.FencingToken)
	if f.head(t) != latest.ID {
		t.Fatal("head did not advance")
	}
	if _, _, err := f.store.RevisionBlob(ctx, f.members[0], latest.ID); err != nil {
		t.Fatalf("head revision was pruned after the clock moved back: %v", err)
	}
}

func TestInvitesAreOneUseAndTimeLimited(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	owner, friend := f.members[0], f.members[1]
	if _, err := f.store.CreateInvite(ctx, friend); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner invite error = %v", err)
	}
	invite, err := f.store.CreateInvite(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.store.JoinGroup(ctx, invite.Code, "c", "PC-c")
	if err != nil {
		t.Fatal(err)
	}
	if joined.Member.Status != proto.MemberPending {
		t.Fatalf("new member status = %s", joined.Member.Status)
	}
	if _, err := f.store.JoinGroup(ctx, invite.Code, "stranger", "x"); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("used invite code accepted again: %v", err)
	}
	stale, _ := f.store.CreateInvite(ctx, owner)
	f.clock = f.clock.Add(InviteLifetime + time.Minute)
	if _, err := f.store.JoinGroup(ctx, stale.Code, "late", "x"); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("expired invite code accepted: %v", err)
	}
	byToken, err := f.store.MemberByToken(ctx, joined.Token)
	if err != nil || byToken.Status != proto.MemberPending {
		t.Fatalf("pending member by token: %+v %v", byToken, err)
	}
	if err := f.store.ApproveMember(ctx, friend, joined.Member.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner approve error = %v", err)
	}
	if err := f.store.ApproveMember(ctx, owner, joined.Member.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ApproveMember(ctx, owner, joined.Member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second approve error = %v", err)
	}
	byToken, _ = f.store.MemberByToken(ctx, joined.Token)
	if byToken.Status != proto.MemberApproved {
		t.Fatalf("status after approval = %s", byToken.Status)
	}
	turnedAway, _ := f.store.CreateInvite(ctx, owner)
	rejected, _ := f.store.JoinGroup(ctx, turnedAway.Code, "d", "PC-d")
	if err := f.store.RevokeMember(ctx, owner, rejected.Member.ID); err != nil {
		t.Fatalf("turning away a pending member: %v", err)
	}
	if _, err := f.store.MemberByToken(ctx, rejected.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("turned away member still has a token: %v", err)
	}
	if err := f.store.ApproveMember(ctx, owner, rejected.Member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a turned away member could be approved afterwards: %v", err)
	}
}

func TestOwnerCanRevokeMembers(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	owner, friend := f.members[0], f.members[1]
	if err := f.store.RevokeMember(ctx, friend, owner.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner revoke error = %v", err)
	}
	if err := f.store.RevokeMember(ctx, owner, owner.ID); !errors.Is(err, ErrSelf) {
		t.Fatalf("self revoke error = %v", err)
	}
	if _, err := f.store.AcquireLease(ctx, friend, f.world.ID, "s"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeMember(ctx, owner, friend.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AcquireLease(ctx, owner, f.world.ID, "s"); err != nil {
		t.Fatalf("revoked member's lease still blocks the world: %v", err)
	}
	me, _ := f.store.Me(ctx, owner)
	if len(me.Members) != 1 {
		t.Fatalf("revoked member still listed: %+v", me.Members)
	}
}

func TestInviteCodeForgivesCommonTypos(t *testing.T) {
	if got := NormalizeInviteCode(" ab0d-e1gh "); got != "ABODEIGH" {
		t.Fatalf("normalized = %q", got)
	}
}

func TestWorldNamesAreUniqueAndDeletionIsRestricted(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	owner, creator, other := f.members[0], f.members[1], f.members[2]
	if _, err := f.store.CreateWorld(ctx, creator, proto.CreateWorldRequest{Name: "BASE"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name error = %v", err)
	}
	second, err := f.store.CreateWorld(ctx, creator, proto.CreateWorldRequest{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.DeleteWorld(ctx, other, second.ID); !errors.Is(err, ErrNotYours) {
		t.Fatalf("stranger delete error = %v", err)
	}
	lease, _ := f.store.AcquireLease(ctx, other, second.ID, "s")
	revision := f.commitTo(t, second.ID, other, "", lease.FencingToken)
	var held *LeaseHeldError
	if _, err := f.store.DeleteWorld(ctx, creator, second.ID); !errors.As(err, &held) {
		t.Fatalf("delete during hosting error = %v", err)
	}
	f.store.ReleaseLease(ctx, other, second.ID, lease.FencingToken)
	blobIDs, err := f.store.DeleteWorld(ctx, owner, second.ID)
	if err != nil || len(blobIDs) != 1 || blobIDs[0] != revision.ID {
		t.Fatalf("owner delete: %v %v", err, blobIDs)
	}
	if _, err := f.store.World(ctx, owner, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("world still exists: %v", err)
	}
}

func TestLostUploadResponseDoesNotForkTheSession(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	first := f.commit(t, f.members[0], "", lease.FencingToken)
	retried, orphaned, err := f.store.Commit(ctx, f.members[0], CommitInput{
		WorldID: f.world.ID, ParentID: "", FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: first.Sha256, Size: 1,
	})
	if err != nil || retried.ID != first.ID || len(orphaned) != 1 {
		t.Fatalf("identical retry: revision %s (want %s) orphaned %v err %v", retried.ID, first.ID, orphaned, err)
	}
	next, _, err := f.store.Commit(ctx, f.members[0], CommitInput{
		WorldID: f.world.ID, ParentID: "", FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: "11", Size: 1,
	})
	if err != nil || next.Branch != proto.MainBranch || next.ParentID != first.ID || f.head(t) != next.ID {
		t.Fatalf("upload with a stale parent under the same lease: %+v err %v", next, err)
	}
	f.store.ReleaseLease(ctx, f.members[0], f.world.ID, lease.FencingToken)
	other, _ := f.store.AcquireLease(ctx, f.members[1], f.world.ID, "s")
	stale := f.commit(t, f.members[1], first.ID, other.FencingToken)
	if stale.Branch == proto.MainBranch {
		t.Fatal("a different host with a stale parent was fast-forwarded onto main")
	}
}

func TestCheckpointsCannotPushSessionSavesOutOfHistory(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	lease, _ := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "s")
	sessionEnd, _, _ := f.store.Commit(ctx, f.members[0], CommitInput{WorldID: f.world.ID, FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: "aa", Size: 1, Note: "session end"})
	f.clock = f.clock.Add(time.Second)
	parentID := sessionEnd.ID
	for index := range 40 {
		revision, _, err := f.store.Commit(ctx, f.members[0], CommitInput{WorldID: f.world.ID, ParentID: parentID, FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: string(rune('b' + index)), Size: 1, Note: CheckpointNote})
		if err != nil {
			t.Fatal(err)
		}
		f.clock = f.clock.Add(time.Second)
		parentID = revision.ID
	}
	if _, _, err := f.store.RevisionBlob(ctx, f.members[0], sessionEnd.ID); err != nil {
		t.Fatalf("a long session's checkpoints pruned the last real save: %v", err)
	}
	revisions, _ := f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	if len(revisions) > 6 {
		t.Fatalf("%d revisions kept, checkpoints are not being pruned", len(revisions))
	}
}

func TestOnlyAuthorOrOwnerDeletesABranchSave(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	owner, author, other := f.members[0], f.members[1], f.members[2]
	fork := f.commit(t, author, "", 0)
	if _, err := f.store.DiscardFork(ctx, other, fork.ID); !errors.Is(err, ErrNotAuthor) {
		t.Fatalf("stranger deleting a friend's branch: %v", err)
	}
	if _, err := f.store.DiscardFork(ctx, owner, fork.ID); err != nil {
		t.Fatalf("owner: %v", err)
	}
}

func TestSameMemberCannotHostFromTwoWindowsAtOnce(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()
	first, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "window-1")
	if err != nil {
		t.Fatal(err)
	}
	var held *LeaseHeldError
	if _, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "window-2"); !errors.As(err, &held) || !held.SameMember {
		t.Fatalf("second window of the same member got the lease: %v", err)
	}
	f.clock = f.clock.Add(ActiveWindow / 2)
	if _, err := f.store.Heartbeat(ctx, f.members[0], f.world.ID, first.FencingToken); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(ActiveWindow / 2)
	if _, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "window-2"); !errors.As(err, &held) {
		t.Fatalf("a heartbeating window was taken over: %v", err)
	}
	f.clock = f.clock.Add(ActiveWindow + time.Second)
	second, err := f.store.AcquireLease(ctx, f.members[0], f.world.ID, "window-2")
	if err != nil {
		t.Fatalf("crashed window blocks the restart: %v", err)
	}
	if second.FencingToken == first.FencingToken {
		t.Fatal("takeover kept the old fencing token, a zombie of the first window could still write main")
	}
	if _, err := f.store.Heartbeat(ctx, f.members[0], f.world.ID, first.FencingToken); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("zombie heartbeat error = %v", err)
	}
}

func TestOwnershipTransferAndRecovery(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	owner, heir, other := f.members[0], f.members[1], f.members[2]
	if err := f.store.TransferOwnership(ctx, heir, other.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner transfer error = %v", err)
	}
	invite, _ := f.store.CreateInvite(ctx, owner)
	pending, _ := f.store.JoinGroup(ctx, invite.Code, "p", "PC")
	if err := f.store.TransferOwnership(ctx, owner, pending.Member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transfer to a pending member error = %v", err)
	}
	if err := f.store.TransferOwnership(ctx, owner, heir.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateInvite(ctx, owner); !errors.Is(err, ErrForbidden) {
		t.Fatal("old owner can still invite")
	}
	if _, err := f.store.CreateInvite(ctx, heir); err != nil {
		t.Fatalf("new owner cannot invite: %v", err)
	}
	groups, err := f.store.ListGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].OwnerName != "b" || groups[0].MemberCount != 4 {
		t.Fatalf("groups = %+v err %v", groups, err)
	}
	recovered, err := f.store.RecoverGroup(ctx, groups[0].ID, "rescuer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateInvite(ctx, recovered.Member); err != nil {
		t.Fatalf("recovered owner cannot invite: %v", err)
	}
	if _, err := f.store.RecoverGroup(ctx, "no-such-group", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recover unknown group error = %v", err)
	}
}

func TestOldDatabaseIsMigratedAndNewerOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	firstRelease := `
CREATE TABLE player_groups (id TEXT PRIMARY KEY, name TEXT NOT NULL, invite_code TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL);
CREATE TABLE members (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, display_name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL);
CREATE TABLE worlds (id TEXT PRIMARY KEY, group_id TEXT NOT NULL, name TEXT NOT NULL, game_name TEXT NOT NULL, default_save_path TEXT NOT NULL,
	default_launch TEXT NOT NULL, default_process TEXT NOT NULL, head_revision_id TEXT NOT NULL DEFAULT '', join_info TEXT NOT NULL DEFAULT '',
	lease_counter INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL);
CREATE TABLE revisions (id TEXT PRIMARY KEY, world_id TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL, blob_id TEXT NOT NULL,
	sha256 TEXT NOT NULL, size INTEGER NOT NULL, author_id TEXT NOT NULL, created_at INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '');
CREATE TABLE leases (world_id TEXT PRIMARY KEY, holder_id TEXT NOT NULL, fencing_token INTEGER NOT NULL, base_revision_id TEXT NOT NULL DEFAULT '',
	acquired_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
INSERT INTO player_groups VALUES ('g1', 'old crew', 'ABCDEFGH', 1);
INSERT INTO members VALUES ('m1', 'g1', 'founder', 'hash1', 1);
INSERT INTO members VALUES ('m2', 'g1', 'friend', 'hash2', 2);
INSERT INTO worlds (id, group_id, name, game_name, default_save_path, default_launch, default_process, created_at) VALUES ('w1', 'g1', 'base', 'valheim', '', '', '', 1);
PRAGMA user_version = 1;`
	if _, err := raw.Exec(firstRelease); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("opening a first-release database: %v", err)
	}
	defer migrated.Close()
	founder := proto.Member{ID: "m1", GroupID: "g1", DisplayName: "founder", Status: proto.MemberApproved}
	me, err := migrated.Me(context.Background(), founder)
	if err != nil || me.Group.OwnerID != "m1" || len(me.Members) != 2 {
		t.Fatalf("after migration: %+v err %v", me, err)
	}
	if _, err := migrated.CreateInvite(context.Background(), founder); err != nil {
		t.Fatalf("first member did not become the owner: %v", err)
	}
	lease, err := migrated.AcquireLease(context.Background(), founder, "w1", "s")
	if err != nil {
		t.Fatalf("lease on a migrated world: %v", err)
	}
	if _, _, err := migrated.Commit(context.Background(), founder, CommitInput{WorldID: "w1", FencingToken: lease.FencingToken, BlobID: NewID(), Sha256: "00", Size: 1}); err != nil {
		t.Fatalf("commit on a migrated world: %v", err)
	}
	migrated.Close()

	raw, _ = sql.Open("sqlite", "file:"+path)
	raw.Exec("PRAGMA user_version = 99")
	raw.Close()
	if _, err := Open(path); !errors.Is(err, ErrNewerDatabase) {
		t.Fatalf("newer database error = %v", err)
	}
}

func TestYoungForksAreNeverPruned(t *testing.T) {
	f := newFixture(t, 2)
	f.store.KeepForks = 1
	ctx := context.Background()
	for range 5 {
		f.commit(t, f.members[1], "", 0)
	}
	revisions, _ := f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	if len(revisions) != 5 {
		t.Fatalf("%d of 5 recent branch saves survived", len(revisions))
	}
	f.clock = f.clock.Add(f.store.ForkGrace + time.Hour)
	f.commit(t, f.members[1], "", 0)
	revisions, _ = f.store.ListRevisions(ctx, f.members[0], f.world.ID)
	if len(revisions) != 2 {
		t.Fatalf("after the grace period %d branch saves remain, want the newest old one plus the new one", len(revisions))
	}
}

func TestRetriedBranchUploadIsNotDuplicated(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	first := f.commitSha(t, f.world.ID, f.members[1], "", 0, "same-content")
	again, orphaned, err := f.store.Commit(ctx, f.members[1], CommitInput{WorldID: f.world.ID, ParentID: "", FencingToken: 0, BlobID: NewID(), Sha256: "same-content", Size: 1})
	if err != nil || again.ID != first.ID || len(orphaned) != 1 {
		t.Fatalf("retry produced %s (first %s), orphaned %v, err %v", again.ID, first.ID, orphaned, err)
	}
	other := f.commitSha(t, f.world.ID, f.members[0], "", 0, "same-content")
	if other.ID == first.ID {
		t.Fatal("another author's identical upload was merged into someone else's branch")
	}
}

func TestRemovingAMemberVoidsTheirOpenInvites(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()
	owner, heir := f.members[0], f.members[1]
	invite, _ := f.store.CreateInvite(ctx, owner)
	if err := f.store.TransferOwnership(ctx, owner, heir.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RevokeMember(ctx, heir, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.JoinGroup(ctx, invite.Code, "late", "PC"); !errors.Is(err, ErrBadInvite) {
		t.Fatalf("a removed member's invite still admits people: %v", err)
	}
}

func TestTheLastSaveByAnotherMemberSurvivesAHostingSpree(t *testing.T) {
	f := newFixture(t, 2)
	f.store.KeepMain = 3
	ctx := context.Background()
	a, d := f.members[0], f.members[1]
	leaseA, _ := f.store.AcquireLease(ctx, a, f.world.ID, "a")
	good := f.commit(t, a, "", leaseA.FencingToken)
	f.store.ReleaseLease(ctx, a, f.world.ID, leaseA.FencingToken)
	parentID := good.ID
	for range 10 {
		leaseD, _ := f.store.AcquireLease(ctx, d, f.world.ID, "d")
		parentID = f.commit(t, d, parentID, leaseD.FencingToken).ID
		f.store.ReleaseLease(ctx, d, f.world.ID, leaseD.FencingToken)
	}
	if _, _, err := f.store.RevisionBlob(ctx, a, good.ID); err != nil {
		t.Fatalf("the last save before d's spree was pruned: %v", err)
	}
	revisions, _ := f.store.ListRevisions(ctx, a, f.world.ID)
	if len(revisions) != f.store.KeepMain+1 {
		t.Fatalf("%d revisions kept, want %d", len(revisions), f.store.KeepMain+1)
	}
	leaseA, _ = f.store.AcquireLease(ctx, a, f.world.ID, "a")
	f.commit(t, a, parentID, leaseA.FencingToken)
	f.store.ReleaseLease(ctx, a, f.world.ID, leaseA.FencingToken)
	if _, _, err := f.store.RevisionBlob(ctx, a, good.ID); err == nil {
		t.Fatal("once a hosts again the old protected save should be subject to normal retention")
	}
}
