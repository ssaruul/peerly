package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"peerly/proto"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrLeaseLost = errors.New("lease is no longer held by this client")
	ErrNotFork   = errors.New("only branch saves can be deleted, saves on main are kept as history")
	ErrIsHead    = errors.New("this save is already the current world")
	ErrForbidden = errors.New("only the group owner can do this")
	ErrNotYours  = errors.New("only the person who added this world or the group owner can delete it")
	ErrNameTaken = errors.New("a world with this name already exists in the group")
	ErrSelf      = errors.New("the owner cannot remove themselves")
	ErrNotAuthor = errors.New("only the person who made this branch save or the group owner can delete it")
	ErrPending   = errors.New("this PC is waiting for the group owner to approve it")
	ErrBadInvite = errors.New("this invite code is unknown, already used or older than a day. Ask the group owner for a new one")
	ErrGone      = errors.New("this save is no longer kept on the server, only its record in History")
)

const (
	InviteLifetime = 24 * time.Hour
	ActiveWindow   = 75 * time.Second
)

type LeaseHeldError struct {
	Lease      proto.Lease
	SameMember bool
}

func (e *LeaseHeldError) Error() string {
	if e.SameMember {
		return "you are already hosting this world from another peerly window or PC. If peerly crashed a moment ago, wait a minute and try again"
	}
	return "world is currently hosted by " + e.Lease.HolderName
}

var ErrNewerDatabase = errors.New("the database was written by a newer peerly server, upgrade this server binary")

type Store struct {
	db              *sql.DB
	Now             func() time.Time
	LeaseTTL        time.Duration
	KeepPeople      int
	KeepCheckpoints int
	KeepForks       int
	ForkGrace       time.Duration
	KeepRecords     time.Duration
}

const CheckpointNote = "checkpoint"

const shrinkFloor = 1 << 20

const ShrunkWarning = "this save is less than half the size of the group's current world, so it was kept as a separate branch instead of replacing it. A world that suddenly shrinks usually means the game started a new world under the old name or could not load the old one. If it really is the newer world, make it current from History"

const schemaVersion = 3

const schema = `
CREATE TABLE IF NOT EXISTS player_groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	owner_id TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS members (
	id TEXT PRIMARY KEY,
	group_id TEXT NOT NULL REFERENCES player_groups(id),
	display_name TEXT NOT NULL,
	device_name TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'approved',
	token_hash TEXT NOT NULL UNIQUE,
	revoked_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS invites (
	code TEXT PRIMARY KEY,
	group_id TEXT NOT NULL REFERENCES player_groups(id),
	created_by TEXT NOT NULL REFERENCES members(id),
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_by TEXT NOT NULL DEFAULT '',
	used_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS worlds (
	id TEXT PRIMARY KEY,
	group_id TEXT NOT NULL REFERENCES player_groups(id),
	name TEXT NOT NULL,
	game_name TEXT NOT NULL,
	default_save_path TEXT NOT NULL,
	default_launch TEXT NOT NULL,
	default_process TEXT NOT NULL,
	default_include TEXT NOT NULL DEFAULT '',
	head_revision_id TEXT NOT NULL DEFAULT '',
	join_info TEXT NOT NULL DEFAULT '',
	lease_counter INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS revisions (
	id TEXT PRIMARY KEY,
	world_id TEXT NOT NULL REFERENCES worlds(id),
	parent_id TEXT NOT NULL DEFAULT '',
	branch TEXT NOT NULL,
	blob_id TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	author_id TEXT NOT NULL REFERENCES members(id),
	created_at INTEGER NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	fencing_token INTEGER NOT NULL DEFAULT 0,
	pruned_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS revisions_world ON revisions(world_id, created_at);
CREATE INDEX IF NOT EXISTS revisions_blob ON revisions(blob_id);
CREATE TABLE IF NOT EXISTS leases (
	world_id TEXT PRIMARY KEY REFERENCES worlds(id),
	holder_id TEXT NOT NULL REFERENCES members(id),
	fencing_token INTEGER NOT NULL,
	base_revision_id TEXT NOT NULL DEFAULT '',
	session_id TEXT NOT NULL DEFAULT '',
	acquired_at INTEGER NOT NULL,
	renewed_at INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NOT NULL
);
`

type column struct {
	table, name, definition string
}

var migrations = map[int]func(ctx context.Context, tx *sql.Tx) error{
	1: func(ctx context.Context, tx *sql.Tx) error {
		for _, added := range []column{
			{"player_groups", "owner_id", "TEXT NOT NULL DEFAULT ''"},
			{"members", "device_name", "TEXT NOT NULL DEFAULT ''"},
			{"members", "status", "TEXT NOT NULL DEFAULT 'approved'"},
			{"members", "revoked_at", "INTEGER NOT NULL DEFAULT 0"},
			{"worlds", "default_include", "TEXT NOT NULL DEFAULT ''"},
			{"worlds", "created_by", "TEXT NOT NULL DEFAULT ''"},
			{"revisions", "fencing_token", "INTEGER NOT NULL DEFAULT 0"},
			{"leases", "session_id", "TEXT NOT NULL DEFAULT ''"},
			{"leases", "renewed_at", "INTEGER NOT NULL DEFAULT 0"},
		} {
			if err := addColumn(ctx, tx, added); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE player_groups SET owner_id = (SELECT id FROM members m WHERE m.group_id = player_groups.id ORDER BY created_at LIMIT 1) WHERE owner_id = ''`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, schema)
		return err
	},
	2: func(ctx context.Context, tx *sql.Tx) error {
		return addColumn(ctx, tx, column{"revisions", "pruned_at", "INTEGER NOT NULL DEFAULT 0"})
	},
}

func addColumn(ctx context.Context, tx *sql.Tx, added column) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+added.table+`)`)
	if err != nil {
		return err
	}
	exists := false
	for rows.Next() {
		var id int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == added.name {
			exists = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, added.table, added.name, added.definition))
	return err
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("%w (database version %d, server version %d)", ErrNewerDatabase, version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schema); err != nil {
			return err
		}
	} else {
		for step := version; step < schemaVersion; step++ {
			if err := migrations[step](ctx, tx); err != nil {
				return fmt.Errorf("migrating the database from version %d: %w", step, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, Now: time.Now, LeaseTTL: 3 * time.Minute, KeepPeople: 5, KeepCheckpoints: 3, KeepForks: 30, ForkGrace: 30 * 24 * time.Hour, KeepRecords: 180 * 24 * time.Hour}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) nowMillis() int64 {
	return s.Now().UnixMilli()
}

func NewID() string {
	buffer := make([]byte, 12)
	rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func newSecret() string {
	buffer := make([]byte, 32)
	rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

func newInviteCode() string {
	buffer := make([]byte, 5)
	rand.Read(buffer)
	return base32.StdEncoding.EncodeToString(buffer)
}

func NormalizeInviteCode(raw string) string {
	replacer := strings.NewReplacer(" ", "", "-", "", "0", "O", "1", "I", "8", "B")
	return replacer.Replace(strings.ToUpper(strings.TrimSpace(raw)))
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) CreateGroup(ctx context.Context, name string, displayName string) (proto.SessionResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	defer tx.Rollback()
	group := proto.Group{ID: NewID(), Name: name}
	if _, err := tx.ExecContext(ctx, `INSERT INTO player_groups (id, name, created_at) VALUES (?, ?, ?)`,
		group.ID, group.Name, s.nowMillis()); err != nil {
		return proto.SessionResponse{}, err
	}
	member, token, err := s.insertMember(ctx, tx, group.ID, displayName, "", proto.MemberApproved)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	group.OwnerID = member.ID
	if _, err := tx.ExecContext(ctx, `UPDATE player_groups SET owner_id = ? WHERE id = ?`, member.ID, group.ID); err != nil {
		return proto.SessionResponse{}, err
	}
	return proto.SessionResponse{Group: group, Member: member, Token: token}, tx.Commit()
}

func (s *Store) CreateInvite(ctx context.Context, owner proto.Member) (proto.Invite, error) {
	group, err := getGroup(ctx, s.db, owner.GroupID)
	if err != nil {
		return proto.Invite{}, err
	}
	if group.OwnerID != owner.ID {
		return proto.Invite{}, ErrForbidden
	}
	invite := proto.Invite{Code: newInviteCode(), ExpiresAt: s.nowMillis() + InviteLifetime.Milliseconds()}
	_, err = s.db.ExecContext(ctx, `INSERT INTO invites (code, group_id, created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		invite.Code, owner.GroupID, owner.ID, s.nowMillis(), invite.ExpiresAt)
	return invite, err
}

func (s *Store) JoinGroup(ctx context.Context, inviteCode string, displayName string, deviceName string) (proto.SessionResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	defer tx.Rollback()
	now := s.nowMillis()
	var groupID string
	err = tx.QueryRowContext(ctx, `SELECT group_id FROM invites WHERE code = ? AND used_at = 0 AND expires_at > ?`,
		NormalizeInviteCode(inviteCode), now).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return proto.SessionResponse{}, ErrBadInvite
	}
	if err != nil {
		return proto.SessionResponse{}, err
	}
	group, err := getGroup(ctx, tx, groupID)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	member, token, err := s.insertMember(ctx, tx, group.ID, displayName, deviceName, proto.MemberPending)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE invites SET used_by = ?, used_at = ? WHERE code = ? AND used_at = 0`,
		member.ID, now, NormalizeInviteCode(inviteCode))
	if err != nil {
		return proto.SessionResponse{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return proto.SessionResponse{}, ErrBadInvite
	}
	return proto.SessionResponse{Group: group, Member: member, Token: token}, tx.Commit()
}

func (s *Store) insertMember(ctx context.Context, tx *sql.Tx, groupID string, displayName string, deviceName string, status string) (proto.Member, string, error) {
	member := proto.Member{ID: NewID(), GroupID: groupID, DisplayName: displayName, DeviceName: deviceName, Status: status, JoinedAt: s.nowMillis()}
	token := newSecret()
	_, err := tx.ExecContext(ctx, `INSERT INTO members (id, group_id, display_name, device_name, status, token_hash, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		member.ID, member.GroupID, member.DisplayName, member.DeviceName, member.Status, hashToken(token), member.JoinedAt)
	return member, token, err
}

const memberColumns = `id, group_id, display_name, device_name, status, created_at`

func scanMember(row interface{ Scan(...any) error }) (proto.Member, error) {
	member := proto.Member{}
	err := row.Scan(&member.ID, &member.GroupID, &member.DisplayName, &member.DeviceName, &member.Status, &member.JoinedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return member, ErrNotFound
	}
	return member, err
}

func (s *Store) MemberByToken(ctx context.Context, token string) (proto.Member, error) {
	return scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE token_hash = ? AND revoked_at = 0`, hashToken(token)))
}

func (s *Store) TransferOwnership(ctx context.Context, owner proto.Member, memberID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	group, err := getGroup(ctx, tx, owner.GroupID)
	if err != nil {
		return err
	}
	if group.OwnerID != owner.ID {
		return ErrForbidden
	}
	target, err := scanMember(tx.QueryRowContext(ctx, `SELECT `+memberColumns+` FROM members WHERE id = ? AND group_id = ? AND revoked_at = 0`, memberID, owner.GroupID))
	if err != nil {
		return err
	}
	if target.Status != proto.MemberApproved {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE player_groups SET owner_id = ? WHERE id = ?`, target.ID, owner.GroupID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListGroups(ctx context.Context) ([]proto.AdminGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.name, COALESCE((SELECT display_name FROM members WHERE id = g.owner_id), ''),
		(SELECT COUNT(*) FROM members WHERE group_id = g.id AND revoked_at = 0),
		(SELECT COUNT(*) FROM worlds WHERE group_id = g.id)
		FROM player_groups g ORDER BY g.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []proto.AdminGroup{}
	for rows.Next() {
		group := proto.AdminGroup{}
		if err := rows.Scan(&group.ID, &group.Name, &group.OwnerName, &group.MemberCount, &group.WorldCount); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) RecoverGroup(ctx context.Context, groupID string, displayName string) (proto.SessionResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	defer tx.Rollback()
	group, err := getGroup(ctx, tx, groupID)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	member, token, err := s.insertMember(ctx, tx, group.ID, displayName, "recovery", proto.MemberApproved)
	if err != nil {
		return proto.SessionResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE player_groups SET owner_id = ? WHERE id = ?`, member.ID, group.ID); err != nil {
		return proto.SessionResponse{}, err
	}
	group.OwnerID = member.ID
	return proto.SessionResponse{Group: group, Member: member, Token: token}, tx.Commit()
}

func (s *Store) BackupTo(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

func (s *Store) ApproveMember(ctx context.Context, owner proto.Member, memberID string) error {
	group, err := getGroup(ctx, s.db, owner.GroupID)
	if err != nil {
		return err
	}
	if group.OwnerID != owner.ID {
		return ErrForbidden
	}
	result, err := s.db.ExecContext(ctx, `UPDATE members SET status = ? WHERE id = ? AND group_id = ? AND status = ? AND revoked_at = 0`,
		proto.MemberApproved, memberID, owner.GroupID, proto.MemberPending)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getGroup(ctx context.Context, q queryer, groupID string) (proto.Group, error) {
	group := proto.Group{}
	err := q.QueryRowContext(ctx, `SELECT id, name, owner_id FROM player_groups WHERE id = ?`, groupID).
		Scan(&group.ID, &group.Name, &group.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return group, ErrNotFound
	}
	return group, err
}

func (s *Store) Me(ctx context.Context, member proto.Member) (proto.MeResponse, error) {
	response := proto.MeResponse{Member: member, Members: []proto.Member{}}
	group, err := getGroup(ctx, s.db, member.GroupID)
	if err != nil {
		return response, err
	}
	response.Group = group
	if member.Status != proto.MemberApproved {
		response.Group.OwnerID = ""
		response.Members = append(response.Members, member)
		return response, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+memberColumns+` FROM members WHERE group_id = ? AND revoked_at = 0 ORDER BY created_at`, member.GroupID)
	if err != nil {
		return response, err
	}
	defer rows.Close()
	for rows.Next() {
		other, err := scanMember(rows)
		if err != nil {
			return response, err
		}
		response.Members = append(response.Members, other)
	}
	return response, rows.Err()
}

func (s *Store) RevokeMember(ctx context.Context, owner proto.Member, memberID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	group, err := getGroup(ctx, tx, owner.GroupID)
	if err != nil {
		return err
	}
	if group.OwnerID != owner.ID {
		return ErrForbidden
	}
	if memberID == owner.ID {
		return ErrSelf
	}
	result, err := tx.ExecContext(ctx, `UPDATE members SET revoked_at = ? WHERE id = ? AND group_id = ? AND revoked_at = 0`,
		s.nowMillis(), memberID, owner.GroupID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE holder_id = ?`, memberID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE invites SET used_by = 'revoked', used_at = ? WHERE created_by = ? AND used_at = 0`, s.nowMillis(), memberID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateWorld(ctx context.Context, member proto.Member, request proto.CreateWorldRequest) (proto.World, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.World{}, err
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM worlds WHERE group_id = ? AND LOWER(name) = LOWER(?)`,
		member.GroupID, request.Name).Scan(&existing); err != nil {
		return proto.World{}, err
	}
	if existing > 0 {
		return proto.World{}, ErrNameTaken
	}
	world := proto.World{
		ID:              NewID(),
		GroupID:         member.GroupID,
		Name:            request.Name,
		GameName:        request.GameName,
		DefaultSavePath: request.DefaultSavePath,
		DefaultLaunch:   request.DefaultLaunch,
		DefaultProcess:  request.DefaultProcess,
		DefaultInclude:  request.DefaultInclude,
		CreatedBy:       member.ID,
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO worlds (id, group_id, name, game_name, default_save_path, default_launch, default_process, default_include, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		world.ID, world.GroupID, world.Name, world.GameName, world.DefaultSavePath, world.DefaultLaunch,
		world.DefaultProcess, world.DefaultInclude, world.CreatedBy, s.nowMillis()); err != nil {
		return proto.World{}, err
	}
	return world, tx.Commit()
}

func (s *Store) DeleteWorld(ctx context.Context, member proto.Member, worldID string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	world, err := getWorld(ctx, tx, worldID, member.GroupID)
	if err != nil {
		return nil, err
	}
	group, err := getGroup(ctx, tx, member.GroupID)
	if err != nil {
		return nil, err
	}
	if world.CreatedBy != member.ID && group.OwnerID != member.ID {
		return nil, ErrNotYours
	}
	lease, found, err := getLease(ctx, tx, worldID)
	if err != nil {
		return nil, err
	}
	if found && lease.ExpiresAt > s.nowMillis() {
		lease.FencingToken = 0
		return nil, &LeaseHeldError{Lease: lease}
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT blob_id FROM revisions WHERE world_id = ? AND blob_id != ''`, worldID)
	if err != nil {
		return nil, err
	}
	blobIDs := []string{}
	for rows.Next() {
		var blobID string
		if err := rows.Scan(&blobID); err != nil {
			rows.Close()
			return nil, err
		}
		blobIDs = append(blobIDs, blobID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, statement := range []string{
		`DELETE FROM leases WHERE world_id = ?`,
		`DELETE FROM revisions WHERE world_id = ?`,
		`DELETE FROM worlds WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, worldID); err != nil {
			return nil, err
		}
	}
	return blobIDs, tx.Commit()
}

const worldColumns = `id, group_id, name, game_name, default_save_path, default_launch, default_process, default_include, head_revision_id, join_info, created_by`

func scanWorld(row interface{ Scan(...any) error }) (proto.World, error) {
	world := proto.World{}
	err := row.Scan(&world.ID, &world.GroupID, &world.Name, &world.GameName, &world.DefaultSavePath,
		&world.DefaultLaunch, &world.DefaultProcess, &world.DefaultInclude, &world.HeadRevisionID, &world.JoinInfo, &world.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return world, ErrNotFound
	}
	return world, err
}

func getWorld(ctx context.Context, q queryer, worldID string, groupID string) (proto.World, error) {
	return scanWorld(q.QueryRowContext(ctx, `SELECT `+worldColumns+` FROM worlds WHERE id = ? AND group_id = ?`, worldID, groupID))
}

func (s *Store) World(ctx context.Context, member proto.Member, worldID string) (proto.World, error) {
	return getWorld(ctx, s.db, worldID, member.GroupID)
}

const revisionColumns = `r.id, r.world_id, r.parent_id, r.branch, r.sha256, r.size, r.author_id, m.display_name, r.created_at, r.note, r.pruned_at`

func scanRevision(row interface{ Scan(...any) error }) (proto.Revision, error) {
	revision := proto.Revision{}
	err := row.Scan(&revision.ID, &revision.WorldID, &revision.ParentID, &revision.Branch, &revision.Sha256,
		&revision.Size, &revision.AuthorID, &revision.AuthorName, &revision.CreatedAt, &revision.Note, &revision.PrunedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return revision, ErrNotFound
	}
	return revision, err
}

func getRevision(ctx context.Context, q queryer, revisionID string) (proto.Revision, error) {
	return scanRevision(q.QueryRowContext(ctx, `SELECT `+revisionColumns+` FROM revisions r JOIN members m ON m.id = r.author_id WHERE r.id = ?`, revisionID))
}

func getLease(ctx context.Context, q queryer, worldID string) (proto.Lease, bool, error) {
	lease := proto.Lease{}
	err := q.QueryRowContext(ctx, `SELECT l.world_id, l.holder_id, m.display_name, l.fencing_token, l.base_revision_id, l.acquired_at, l.expires_at
		FROM leases l JOIN members m ON m.id = l.holder_id WHERE l.world_id = ?`, worldID).
		Scan(&lease.WorldID, &lease.HolderID, &lease.HolderName, &lease.FencingToken, &lease.BaseRevisionID, &lease.AcquiredAt, &lease.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lease, false, nil
	}
	return lease, err == nil, err
}

func getLeaseSession(ctx context.Context, q queryer, worldID string) (sessionID string, renewedAt int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT session_id, renewed_at FROM leases WHERE world_id = ?`, worldID).Scan(&sessionID, &renewedAt)
	return sessionID, renewedAt, err
}

func (s *Store) ListWorlds(ctx context.Context, member proto.Member) ([]proto.WorldStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+worldColumns+` FROM worlds WHERE group_id = ? ORDER BY created_at`, member.GroupID)
	if err != nil {
		return nil, err
	}
	worlds := []proto.World{}
	for rows.Next() {
		world, err := scanWorld(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		worlds = append(worlds, world)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.nowMillis()
	statuses := []proto.WorldStatus{}
	for _, world := range worlds {
		status := proto.WorldStatus{World: world}
		if world.HeadRevisionID != "" {
			head, err := getRevision(ctx, s.db, world.HeadRevisionID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
			if err == nil {
				status.Head = &head
			}
		}
		lease, found, err := getLease(ctx, s.db, world.ID)
		if err != nil {
			return nil, err
		}
		if found && lease.ExpiresAt > now {
			if lease.HolderID != member.ID {
				lease.FencingToken = 0
			}
			status.Lease = &lease
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *Store) AcquireLease(ctx context.Context, member proto.Member, worldID string, sessionID string) (proto.Lease, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.Lease{}, err
	}
	defer tx.Rollback()
	world, err := getWorld(ctx, tx, worldID, member.GroupID)
	if err != nil {
		return proto.Lease{}, err
	}
	now := s.nowMillis()
	expiresAt := now + s.LeaseTTL.Milliseconds()
	existing, found, err := getLease(ctx, tx, worldID)
	if err != nil {
		return proto.Lease{}, err
	}
	if found && existing.ExpiresAt > now {
		if existing.HolderID != member.ID {
			existing.FencingToken = 0
			return proto.Lease{}, &LeaseHeldError{Lease: existing}
		}
		heldBySession, renewedAt, err := getLeaseSession(ctx, tx, worldID)
		if err != nil {
			return proto.Lease{}, err
		}
		if heldBySession == sessionID {
			if _, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at = ?, renewed_at = ?, base_revision_id = ? WHERE world_id = ?`,
				expiresAt, now, world.HeadRevisionID, worldID); err != nil {
				return proto.Lease{}, err
			}
			existing.ExpiresAt = expiresAt
			existing.BaseRevisionID = world.HeadRevisionID
			return existing, tx.Commit()
		}
		if now-renewedAt < ActiveWindow.Milliseconds() {
			existing.FencingToken = 0
			return proto.Lease{}, &LeaseHeldError{Lease: existing, SameMember: true}
		}
	}
	var fencingToken int64
	if err := tx.QueryRowContext(ctx, `UPDATE worlds SET lease_counter = lease_counter + 1, join_info = '' WHERE id = ? RETURNING lease_counter`,
		worldID).Scan(&fencingToken); err != nil {
		return proto.Lease{}, err
	}
	lease := proto.Lease{
		WorldID:        worldID,
		HolderID:       member.ID,
		HolderName:     member.DisplayName,
		FencingToken:   fencingToken,
		BaseRevisionID: world.HeadRevisionID,
		AcquiredAt:     now,
		ExpiresAt:      expiresAt,
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO leases (world_id, holder_id, fencing_token, base_revision_id, session_id, acquired_at, renewed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.WorldID, lease.HolderID, lease.FencingToken, lease.BaseRevisionID, sessionID, lease.AcquiredAt, now, lease.ExpiresAt); err != nil {
		return proto.Lease{}, err
	}
	return lease, tx.Commit()
}

func (s *Store) Heartbeat(ctx context.Context, member proto.Member, worldID string, fencingToken int64) (proto.Lease, error) {
	now := s.nowMillis()
	expiresAt := now + s.LeaseTTL.Milliseconds()
	result, err := s.db.ExecContext(ctx, `UPDATE leases SET expires_at = ?, renewed_at = ? WHERE world_id = ? AND holder_id = ? AND fencing_token = ?`,
		expiresAt, now, worldID, member.ID, fencingToken)
	if err != nil {
		return proto.Lease{}, err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return proto.Lease{}, ErrLeaseLost
	}
	lease, _, err := getLease(ctx, s.db, worldID)
	return lease, err
}

func (s *Store) ReleaseLease(ctx context.Context, member proto.Member, worldID string, fencingToken int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE world_id = ? AND holder_id = ? AND fencing_token = ?`,
		worldID, member.ID, fencingToken)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE worlds SET join_info = '' WHERE id = ?`, worldID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetJoinInfo(ctx context.Context, member proto.Member, worldID string, fencingToken int64, joinInfo string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lease, found, err := getLease(ctx, tx, worldID)
	if err != nil {
		return err
	}
	if !found || lease.HolderID != member.ID || lease.FencingToken != fencingToken {
		return ErrLeaseLost
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worlds SET join_info = ? WHERE id = ?`, joinInfo, worldID); err != nil {
		return err
	}
	return tx.Commit()
}

type CommitInput struct {
	WorldID      string
	ParentID     string
	FencingToken int64
	BlobID       string
	Sha256       string
	Size         int64
	Note         string
}

func forkBranchName(displayName string, at time.Time) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '-'
		}
		return r
	}, displayName)
	return fmt.Sprintf("fork/%s/%s", cleaned, at.UTC().Format("20060102-150405"))
}

func (s *Store) pruneRevisions(ctx context.Context, tx *sql.Tx, keepRecord bool, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	type expired struct{ revisionID, blobID string }
	expiredRevisions := []expired{}
	for rows.Next() {
		entry := expired{}
		if err := rows.Scan(&entry.revisionID, &entry.blobID); err != nil {
			rows.Close()
			return nil, err
		}
		expiredRevisions = append(expiredRevisions, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	orphanedBlobs := []string{}
	for _, entry := range expiredRevisions {
		if keepRecord {
			if _, err := tx.ExecContext(ctx, `UPDATE revisions SET blob_id = '', size = 0, pruned_at = ? WHERE id = ?`, s.nowMillis(), entry.revisionID); err != nil {
				return nil, err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE id = ?`, entry.revisionID); err != nil {
			return nil, err
		}
		var remaining int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE blob_id = ?`, entry.blobID).Scan(&remaining); err != nil {
			return nil, err
		}
		if remaining == 0 {
			orphanedBlobs = append(orphanedBlobs, entry.blobID)
		}
	}
	return orphanedBlobs, nil
}

func (s *Store) prune(ctx context.Context, tx *sql.Tx, worldID string, headID string) ([]string, error) {
	orphanedMain, err := s.pruneRevisions(ctx, tx, true, `WITH sessions AS (
			SELECT id, blob_id, author_id,
				ROW_NUMBER() OVER (PARTITION BY author_id ORDER BY created_at DESC, rowid DESC) AS per_person
			FROM revisions WHERE world_id = ? AND branch = ? AND note != ? AND blob_id != ''
		), people AS (
			SELECT author_id FROM revisions WHERE world_id = ? AND branch = ? AND note != ? AND blob_id != ''
			GROUP BY author_id ORDER BY MAX(created_at) DESC LIMIT ?
		)
		SELECT id, blob_id FROM sessions WHERE id != ? AND (per_person > 1 OR author_id NOT IN (SELECT author_id FROM people))`,
		worldID, proto.MainBranch, CheckpointNote, worldID, proto.MainBranch, CheckpointNote, s.KeepPeople, headID)
	if err != nil {
		return nil, err
	}
	orphanedCheckpoints, err := s.pruneRevisions(ctx, tx, false, `SELECT id, blob_id FROM revisions WHERE world_id = ? AND branch = ? AND id != ? AND note = ? AND blob_id != ''
		ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?`, worldID, proto.MainBranch, headID, CheckpointNote, s.KeepCheckpoints)
	if err != nil {
		return nil, err
	}
	orphanedMain = append(orphanedMain, orphanedCheckpoints...)
	orphanedForks, err := s.pruneRevisions(ctx, tx, true, `SELECT id, blob_id FROM revisions WHERE world_id = ? AND branch != ? AND created_at <= ? AND blob_id != ''
		ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?`, worldID, proto.MainBranch, s.nowMillis()-s.ForkGrace.Milliseconds(), s.KeepForks)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE world_id = ? AND blob_id = '' AND pruned_at < ?`, worldID, s.nowMillis()-s.KeepRecords.Milliseconds()); err != nil {
		return nil, err
	}
	return append(orphanedMain, orphanedForks...), nil
}

func (s *Store) Commit(ctx context.Context, member proto.Member, input CommitInput) (proto.Revision, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	defer tx.Rollback()
	world, err := getWorld(ctx, tx, input.WorldID, member.GroupID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	lease, found, err := getLease(ctx, tx, input.WorldID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	var duplicateID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM revisions WHERE world_id = ? AND author_id = ? AND parent_id = ? AND sha256 = ? AND blob_id != '' ORDER BY created_at DESC LIMIT 1`,
		input.WorldID, member.ID, input.ParentID, input.Sha256).Scan(&duplicateID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return proto.Revision{}, nil, err
	}
	if duplicateID != "" {
		duplicate, err := getRevision(ctx, tx, duplicateID)
		if err != nil {
			return proto.Revision{}, nil, err
		}
		return duplicate, []string{input.BlobID}, tx.Commit()
	}
	holdsLease := found && lease.HolderID == member.ID && lease.FencingToken == input.FencingToken && input.FencingToken != 0
	parentID := input.ParentID
	if holdsLease && parentID != world.HeadRevisionID && world.HeadRevisionID != "" {
		var headToken int64
		var headAuthor, headSha256 string
		if err := tx.QueryRowContext(ctx, `SELECT fencing_token, author_id, sha256 FROM revisions WHERE id = ?`, world.HeadRevisionID).
			Scan(&headToken, &headAuthor, &headSha256); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return proto.Revision{}, nil, err
		}
		if headToken == lease.FencingToken && headAuthor == member.ID {
			if headSha256 == input.Sha256 {
				head, err := getRevision(ctx, tx, world.HeadRevisionID)
				if err != nil {
					return proto.Revision{}, nil, err
				}
				return head, []string{input.BlobID}, tx.Commit()
			}
			parentID = world.HeadRevisionID
		}
	}
	branch := proto.MainBranch
	warning := ""
	if holdsLease && parentID == world.HeadRevisionID && world.HeadRevisionID != "" {
		var headSize int64
		if err := tx.QueryRowContext(ctx, `SELECT size FROM revisions WHERE id = ?`, world.HeadRevisionID).Scan(&headSize); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return proto.Revision{}, nil, err
		}
		if headSize > shrinkFloor && input.Size*2 < headSize {
			branch = forkBranchName(member.DisplayName, s.Now())
			warning = ShrunkWarning
		}
	}
	if !holdsLease || parentID != world.HeadRevisionID {
		branch = forkBranchName(member.DisplayName, s.Now())
		if input.ParentID != "" {
			parent, err := getRevision(ctx, tx, input.ParentID)
			if err == nil && parent.WorldID == input.WorldID && parent.AuthorID == member.ID && parent.Branch != proto.MainBranch {
				branch = parent.Branch
			}
		}
	}
	revision := proto.Revision{
		ID:         input.BlobID,
		WorldID:    input.WorldID,
		ParentID:   parentID,
		Branch:     branch,
		Sha256:     input.Sha256,
		Size:       input.Size,
		AuthorID:   member.ID,
		AuthorName: member.DisplayName,
		CreatedAt:  s.nowMillis(),
		Note:       input.Note,
	}
	storedToken := int64(0)
	if branch == proto.MainBranch {
		storedToken = lease.FencingToken
	}
	if err := insertRevision(ctx, tx, revision, input.BlobID, storedToken); err != nil {
		return proto.Revision{}, nil, err
	}
	headID := world.HeadRevisionID
	if branch == proto.MainBranch {
		headID = revision.ID
		if _, err := tx.ExecContext(ctx, `UPDATE worlds SET head_revision_id = ? WHERE id = ?`, revision.ID, input.WorldID); err != nil {
			return proto.Revision{}, nil, err
		}
	}
	orphanedBlobs, err := s.prune(ctx, tx, input.WorldID, headID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	revision.Warning = warning
	return revision, orphanedBlobs, tx.Commit()
}

func insertRevision(ctx context.Context, tx *sql.Tx, revision proto.Revision, blobID string, fencingToken int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO revisions (id, world_id, parent_id, branch, blob_id, sha256, size, author_id, created_at, note, fencing_token)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		revision.ID, revision.WorldID, revision.ParentID, revision.Branch, blobID, revision.Sha256,
		revision.Size, revision.AuthorID, revision.CreatedAt, revision.Note, fencingToken)
	return err
}

func (s *Store) Promote(ctx context.Context, member proto.Member, worldID string, revisionID string) (proto.Revision, []string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	defer tx.Rollback()
	world, err := getWorld(ctx, tx, worldID, member.GroupID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	if world.HeadRevisionID == revisionID {
		return proto.Revision{}, nil, ErrIsHead
	}
	lease, found, err := getLease(ctx, tx, worldID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	if found && lease.ExpiresAt > s.nowMillis() {
		lease.FencingToken = 0
		return proto.Revision{}, nil, &LeaseHeldError{Lease: lease}
	}
	source, err := getRevision(ctx, tx, revisionID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	if source.WorldID != worldID {
		return proto.Revision{}, nil, ErrNotFound
	}
	var blobID string
	if err := tx.QueryRowContext(ctx, `SELECT blob_id FROM revisions WHERE id = ?`, revisionID).Scan(&blobID); err != nil {
		return proto.Revision{}, nil, err
	}
	if blobID == "" {
		return proto.Revision{}, nil, ErrGone
	}
	promoted := proto.Revision{
		ID:         NewID(),
		WorldID:    worldID,
		ParentID:   world.HeadRevisionID,
		Branch:     proto.MainBranch,
		Sha256:     source.Sha256,
		Size:       source.Size,
		AuthorID:   member.ID,
		AuthorName: member.DisplayName,
		CreatedAt:  s.nowMillis(),
		Note:       fmt.Sprintf("made current from %s by %s", source.Branch, source.AuthorName),
	}
	if err := insertRevision(ctx, tx, promoted, blobID, 0); err != nil {
		return proto.Revision{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE worlds SET head_revision_id = ? WHERE id = ?`, promoted.ID, worldID); err != nil {
		return proto.Revision{}, nil, err
	}
	orphanedBlobs, err := s.prune(ctx, tx, worldID, promoted.ID)
	if err != nil {
		return proto.Revision{}, nil, err
	}
	return promoted, orphanedBlobs, tx.Commit()
}

func (s *Store) ListRevisions(ctx context.Context, member proto.Member, worldID string) ([]proto.Revision, error) {
	if _, err := getWorld(ctx, s.db, worldID, member.GroupID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+revisionColumns+` FROM revisions r JOIN members m ON m.id = r.author_id
		WHERE r.world_id = ? ORDER BY r.created_at DESC, r.rowid DESC`, worldID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []proto.Revision{}
	for rows.Next() {
		revision, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

func (s *Store) RevisionBlob(ctx context.Context, member proto.Member, revisionID string) (proto.Revision, string, error) {
	revision, err := getRevision(ctx, s.db, revisionID)
	if err != nil {
		return revision, "", err
	}
	if _, err := getWorld(ctx, s.db, revision.WorldID, member.GroupID); err != nil {
		return revision, "", err
	}
	var blobID string
	err = s.db.QueryRowContext(ctx, `SELECT blob_id FROM revisions WHERE id = ?`, revisionID).Scan(&blobID)
	if errors.Is(err, sql.ErrNoRows) {
		return revision, "", ErrNotFound
	}
	if err == nil && blobID == "" {
		return revision, "", ErrGone
	}
	return revision, blobID, err
}

func (s *Store) DiscardFork(ctx context.Context, member proto.Member, revisionID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	revision, err := getRevision(ctx, tx, revisionID)
	if err != nil {
		return "", err
	}
	if _, err := getWorld(ctx, tx, revision.WorldID, member.GroupID); err != nil {
		return "", err
	}
	if revision.Branch == proto.MainBranch {
		return "", ErrNotFork
	}
	group, err := getGroup(ctx, tx, member.GroupID)
	if err != nil {
		return "", err
	}
	if revision.AuthorID != member.ID && group.OwnerID != member.ID {
		return "", ErrNotAuthor
	}
	var blobID string
	if err := tx.QueryRowContext(ctx, `SELECT blob_id FROM revisions WHERE id = ?`, revisionID).Scan(&blobID); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE id = ?`, revisionID); err != nil {
		return "", err
	}
	if blobID == "" {
		return "", tx.Commit()
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions WHERE blob_id = ?`, blobID).Scan(&remaining); err != nil {
		return "", err
	}
	if remaining > 0 {
		blobID = ""
	}
	return blobID, tx.Commit()
}

func (s *Store) ReferencedBlobs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT blob_id FROM revisions WHERE blob_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	referenced := map[string]bool{}
	for rows.Next() {
		var blobID string
		if err := rows.Scan(&blobID); err != nil {
			return nil, err
		}
		referenced[blobID] = true
	}
	return referenced, rows.Err()
}
