package proto

const MainBranch = "main"

const (
	MemberPending  = "pending"
	MemberApproved = "approved"
)

type Group struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
}

type Member struct {
	ID          string `json:"id"`
	GroupID     string `json:"group_id"`
	DisplayName string `json:"display_name"`
	DeviceName  string `json:"device_name"`
	Status      string `json:"status"`
	JoinedAt    int64  `json:"joined_at"`
}

type Invite struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

type World struct {
	ID              string `json:"id"`
	GroupID         string `json:"group_id"`
	Name            string `json:"name"`
	GameName        string `json:"game_name"`
	DefaultSavePath string `json:"default_save_path"`
	DefaultLaunch   string `json:"default_launch"`
	DefaultProcess  string `json:"default_process"`
	DefaultInclude  string `json:"default_include"`
	HeadRevisionID  string `json:"head_revision_id"`
	JoinInfo        string `json:"join_info"`
	CreatedBy       string `json:"created_by"`
}

type Revision struct {
	ID         string `json:"id"`
	WorldID    string `json:"world_id"`
	ParentID   string `json:"parent_id"`
	Branch     string `json:"branch"`
	Sha256     string `json:"sha256"`
	Size       int64  `json:"size"`
	AuthorID   string `json:"author_id"`
	AuthorName string `json:"author_name"`
	CreatedAt  int64  `json:"created_at"`
	Note       string `json:"note"`
	PrunedAt   int64  `json:"pruned_at,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type Lease struct {
	WorldID        string `json:"world_id"`
	HolderID       string `json:"holder_id"`
	HolderName     string `json:"holder_name"`
	FencingToken   int64  `json:"fencing_token,omitempty"`
	BaseRevisionID string `json:"base_revision_id"`
	AcquiredAt     int64  `json:"acquired_at"`
	ExpiresAt      int64  `json:"expires_at"`
}

type WorldStatus struct {
	World World     `json:"world"`
	Head  *Revision `json:"head"`
	Lease *Lease    `json:"lease"`
}

type CreateGroupRequest struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type JoinGroupRequest struct {
	InviteCode  string `json:"invite_code"`
	DisplayName string `json:"display_name"`
	DeviceName  string `json:"device_name"`
}

type SessionResponse struct {
	Group  Group  `json:"group"`
	Member Member `json:"member"`
	Token  string `json:"token"`
}

type MeResponse struct {
	Group   Group    `json:"group"`
	Member  Member   `json:"member"`
	Members []Member `json:"members"`
}

type CreateWorldRequest struct {
	Name            string `json:"name"`
	GameName        string `json:"game_name"`
	DefaultSavePath string `json:"default_save_path"`
	DefaultLaunch   string `json:"default_launch"`
	DefaultProcess  string `json:"default_process"`
	DefaultInclude  string `json:"default_include"`
}

type LeaseRequest struct {
	FencingToken int64 `json:"fencing_token"`
}

type AcquireRequest struct {
	SessionID string `json:"session_id"`
}

type TransferRequest struct {
	MemberID string `json:"member_id"`
}

type AdminGroup struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OwnerName   string `json:"owner_name"`
	MemberCount int    `json:"member_count"`
	WorldCount  int    `json:"world_count"`
}

type RecoverRequest struct {
	DisplayName string `json:"display_name"`
}

type PromoteRequest struct {
	RevisionID string `json:"revision_id"`
}

type JoinInfoRequest struct {
	FencingToken int64  `json:"fencing_token"`
	JoinInfo     string `json:"join_info"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Lease   *Lease `json:"lease,omitempty"`
	Pending bool   `json:"pending,omitempty"`
}
