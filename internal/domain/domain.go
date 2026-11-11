// Package domain holds the types and ports shared by every omegacd package.
// Terms follow GLOSSARY.md: Application, Host, Repository, Revision, Sync, ...
package domain

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Application kinds.
const (
	KindCompose = "compose"
	KindApps    = "apps"
)

// ManagedLabel is set on every container omegacd creates; its value is the Application name.
const ManagedLabel = "dev.omegacd.application"

// ManagedByLabel=ManagedByValue marks every container OmegaCD creates, for people and other tools filtering on it.
const (
	ManagedByLabel = "managed-by"
	ManagedByValue = "omegacd"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrInvalid       = errors.New("invalid")
	ErrUnmanaged     = errors.New("compose project exists and is not managed by omegacd")
	ErrBusy          = errors.New("a sync operation is already running")
	// ErrWaitingForHost: the delete is recorded and done when the Application's Host's Agent is back (Q83)
	ErrWaitingForHost = errors.New("waiting for the Host")
)

type Application struct {
	Name           string   `json:"name"`
	RepoURL        string   `json:"repoURL"`
	Path           string   `json:"path"`           // directory inside the repo, "" or "." for root
	TargetRevision string   `json:"targetRevision"` // branch, tag or commit SHA; default "main"
	ComposeFiles   []string `json:"composeFiles"`   // relative to Path; empty: the first of DefaultComposeFiles found
	Profiles       []string `json:"profiles"`
	Host           string   `json:"host"` // the Host whose Agent deploys it; none for an App of Apps
	AutoSync       bool     `json:"autoSync"`
	SelfHeal       bool     `json:"selfHeal"`
	Prune          bool     `json:"prune"`
	PruneVolumes   bool     `json:"pruneVolumes"` // with Prune, also delete undeclared project volumes; false whenever Prune is
	Adopt          bool     `json:"adopt"`        // only honoured on create
	// Deleting says how it's being deleted while its Agent hasn't confirmed (Q83): "containers", "volumes" or "keep"
	Deleting   string    `json:"deleting,omitempty"`
	RolledBack bool      `json:"rolledBack"`        // Auto-Sync was turned off by a Rollback; cleared when Auto-Sync is re-enabled
	Kind       string    `json:"kind"`              // KindCompose (default) or KindApps (an App of Apps)
	Parent     string    `json:"parent,omitempty"`  // the App of Apps that owns this Child Application
	NotInGit   bool      `json:"notInGit"`          // a Child Application its parent no longer declares (kept because Prune is off)
	WorkDir    string    `json:"workDir,omitempty"` // absolute root for its folder on the Host (Q57); "": the Host's work dir
	CreatedAt  time.Time `json:"createdAt"`
}

type SyncStatus string

const (
	Synced    SyncStatus = "Synced"
	OutOfSync SyncStatus = "OutOfSync"
	Unknown   SyncStatus = "Unknown"
)

type HealthStatus string

const (
	Healthy     HealthStatus = "Healthy"
	Progressing HealthStatus = "Progressing"
	Degraded    HealthStatus = "Degraded"
	Missing     HealthStatus = "Missing"
	// HealthUnknown: the Host could not be asked (unreachable), so nothing is known about the containers.
	HealthUnknown HealthStatus = "Unknown"
)

type OpPhase string

const (
	OpRunning   OpPhase = "Running"
	OpSucceeded OpPhase = "Succeeded"
	OpFailed    OpPhase = "Failed"
	// a Sync to an offline Host: it runs when its Agent reconnects (Q81), unless a newer one supersedes it first
	OpWaiting    OpPhase = "Waiting"
	OpSuperseded OpPhase = "Superseded"
)

// SyncOperation is one attempt to Sync an Application to a Revision.
type SyncOperation struct {
	ID         int64     `json:"id"`
	App        string    `json:"app"`
	Revision   string    `json:"revision"` // resolved commit SHA
	TreeHash   string    `json:"treeHash"` // git tree hash of the app path at Revision
	Phase      OpPhase   `json:"phase"`
	Message    string    `json:"message"`
	Trigger    string    `json:"trigger"` // "manual", "auto", "self-heal", "rollback"
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// RepoCredential applies to every Repository whose URL starts with URLPrefix.
type RepoCredential struct {
	ID            int64  `json:"id"`
	URLPrefix     string `json:"urlPrefix"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"-"` // password or token
	SSHPrivateKey string `json:"-"`
	HasPassword   bool   `json:"hasPassword"`
	HasSSHKey     bool   `json:"hasSSHKey"`
}

// RepoAccess is whether omegacd can read a Repository. Access is one of
// "public" (readable without credentials), "credential" (readable with the RepoCredential at CredentialPrefix),
// "denied" (needs credentials and none matched, or the matching one was rejected) or
// "error" (network, DNS or URL problems). Message explains denied and error; it never holds secrets.
type RepoAccess struct {
	Access           string `json:"access"`
	CredentialPrefix string `json:"credentialPrefix,omitempty"`
	DefaultBranch    string `json:"defaultBranch,omitempty"`
	Message          string `json:"message,omitempty"`
}

// DefaultComposeFiles are looked for, in this order, when an Application names no compose files — the same
// names docker compose looks for.
var DefaultComposeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// ComposeFilesIn is the Application's compose files in dir: the ones it names, else the first of
// DefaultComposeFiles that exists there (compose.yaml when none does, so errors name a file).
func (a Application) ComposeFilesIn(dir string) []string {
	if len(a.ComposeFiles) > 0 {
		return a.ComposeFiles
	}
	for _, f := range DefaultComposeFiles {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil && fi.Mode().IsRegular() {
			return []string{f}
		}
	}
	return DefaultComposeFiles[:1]
}

// Dir is where the Application's files live in a work dir, as a slash path: <name>, or <parent>/<name> for a
// Child Application. Names are unique, so no two Applications share a folder; an App of Apps has no files.
func (a Application) Dir() string { return path.Join(a.Parent, a.Name) }

// MarkerFile marked the folders OmegaCD created, holding the name of their Application, before an Agent recorded them.
// A Sync still takes a folder marked for its Application (and deletes the marker) and refuses one marked for another.
const MarkerFile = ".omegacd"

// Host is a machine Applications deploy to, through its Agent (Q9).
type Host struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	// an enrolled Agent's (Q22): its ed25519 public key, empty until it enrolls; its age Host Key's recipient; and
	// whether an admin approved the key, comparing Fingerprint (Q28, Q74)
	SigningKey   []byte `json:"-"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	AgeRecipient string `json:"ageRecipient,omitempty"`
	Approved     bool   `json:"approved"`
	// Locked holds the two live connections that used the Host's key at once (Q28, Q75): until an admin unlocks the
	// Host or revokes it (deletes it), its Agent is refused. Its Applications keep running.
	Locked []AgentConnection `json:"locked,omitempty"`
	// AllowIPs, when set, are the only addresses the Host's Agent may connect from (Q28), as CIDRs.
	AllowIPs []string `json:"allowIPs,omitempty"`
	// Agent is what the Host's Agent said about itself when it last connected (Q41); nil before.
	Agent *AgentInfo `json:"agent,omitempty"`
}

// AgentInfo is an Agent's Hello, as last received.
type AgentInfo struct {
	Version       string    `json:"version"`
	Protocol      uint32    `json:"protocol"`
	Hostname      string    `json:"hostname,omitempty"`
	OS            string    `json:"os,omitempty"`
	Arch          string    `json:"arch,omitempty"`
	Engine        string    `json:"engine,omitempty"` // docker or podman
	EngineVersion string    `json:"engineVersion,omitempty"`
	Rootless      bool      `json:"rootless,omitempty"`
	SeenAt        time.Time `json:"seenAt"`
	LeftAt        time.Time `json:"leftAt,omitzero"` // when its connection ended; zero while it's connected
	// Refused says why the Control Plane refused it, such as an outdated version; empty when it was served
	Refused string `json:"refused,omitempty"`
}

// ParsePrefixes parses CIDRs or bare IPs (a bare IP is the prefix of that one address).
func ParsePrefixes(fields []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range fields {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if strings.Contains(f, "/") {
			p, err := netip.ParsePrefix(f)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// AgentReport is a Sync an Agent reports: the outcome of one the Control Plane sent (Op), or one it ran itself (Op 0:
// a restore, a Self-Heal, a rollback on the Host, with Trigger saying which).
type AgentReport struct {
	App, Revision, Checksum string
	Phase                   OpPhase
	Message, Trigger        string
	Op                      int64
	At                      time.Time
}

// AgentStatus is an Application's state as its Host's Agent last reported it (M4-02, Q23): the Control Plane's view of
// it, online and, with its time, while the Host is offline (Q38).
type AgentStatus struct {
	Revision string
	Health   HealthStatus
	Drifted  bool
	Message  string
	Live     LiveState
	Drift    []ServiceDiff
	Details  map[string]map[string]string // "kind/name" of each volume and network → its details
	At       time.Time
}

// AgentDiff is how a Revision not deployed would change what runs, as the Application's Agent computed it (M4-11): what
// the Diff view shows while the Host is offline.
type AgentDiff struct {
	Revision string
	Services []ServiceDiff
	At       time.Time
}

// AgentConnection is where and since when an Agent was connected.
type AgentConnection struct {
	Address string    `json:"address"`
	Since   time.Time `json:"since"`
}

// Fingerprint is how an admin compares an Agent's signing key with what the Agent prints: "SHA256:" and the unpadded
// base64 of the key's sha256, as ssh-keygen -l shows keys.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// HostStatus is a Host's state as its Agent last reported it.
type HostStatus struct {
	Name          string `json:"name"`
	Connected     bool   `json:"connected"`
	Error         string `json:"error,omitempty"`
	ServerVersion string `json:"serverVersion,omitempty"`
	OS            string `json:"os,omitempty"`
	Containers    int    `json:"containers"`
}

// HostView is a Host plus its connection status and the Applications deployed to it, as returned by the API.
type HostView struct {
	Host
	Status HostStatus `json:"status"`
	Apps   []string   `json:"apps"`
}

// HostKey is the public half of a Host's age Host Key, born on its Agent (Q4, Q108): its Encrypted Secrets are
// encrypted for Recipient. The private key never leaves the Agent.
type HostKey struct {
	Recipient string    `json:"recipient"` // age1...
	Current   bool      `json:"current"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
}

// WrongKeyError is returned by Secrets.DecryptDir when none of the given identities can decrypt File.
type WrongKeyError struct {
	File       string   // relative to the decrypted dir
	Recipients []string // the keys File is encrypted for
}

func (e *WrongKeyError) Error() string {
	return e.File + " is encrypted for " + strings.Join(e.Recipients, ", ")
}

type RegistryCredential struct {
	ID          int64  `json:"id"`
	Registry    string `json:"registry"` // host, e.g. ghcr.io
	Username    string `json:"username"`
	Password    string `json:"-"`
	HasPassword bool   `json:"hasPassword"`
}

type APIToken struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	LastUsed  time.Time `json:"lastUsed,omitzero"`
}

// ServiceDiff is one service whose Desired State differs from its Live State.
// FieldStopped marks a "removed" ServiceDiff whose containers are already stopped: only Prune deletes them.
const FieldStopped = "stopped"

// SyncRequest overrides Application settings for one Sync Operation.
type SyncRequest struct {
	Prune *bool `json:"prune,omitempty"`
}

// A ServiceDiff with a Kind is an undeclared project "network" or "volume" (Service is its Docker name,
// Change "removed"): Prune (with Prune volumes, for volumes) would delete it unless a container still uses it.
// It is never drift.
type ServiceDiff struct {
	Kind    string   `json:"kind,omitempty"` // "" for a service, "network", "volume"
	Service string   `json:"service"`
	Change  string   `json:"change"`           // "added", "removed", "changed"
	Fields  []string `json:"fields,omitempty"` // e.g. "image", "environment", "ports"
	Host    string   `json:"host,omitempty"`   // a Child Application's Host, in an App of Apps' diff
}

type Container struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Service  string       `json:"service"`
	Image    string       `json:"image"`
	State    string       `json:"state"`  // docker state: running, exited, restarting, ...
	Status   string       `json:"status"` // human docker status line
	ExitCode int          `json:"exitCode"`
	Health   HealthStatus `json:"health"`
}

// Resource is a network or volume on the Host, or a file the Application's services read:
// "env_file", "config", "secret" or "bind" (Name is then relative to the working dir).
type Resource struct {
	Kind     string   `json:"kind"` // "network", "volume", "env_file", "config", "secret", "bind"
	Name     string   `json:"name"`
	Services []string `json:"services,omitempty"` // services using it
	Target   string   `json:"target,omitempty"`   // mount path in the container
	Secret   bool     `json:"secret,omitempty"`   // content is never revealed
}

type ResourceDetail struct {
	Resource
	Content string            `json:"content,omitempty"`
	Masked  bool              `json:"masked,omitempty"` // Content has its values replaced
	Info    map[string]string `json:"info,omitempty"`
}

// LiveState is what runs on the Host for an Application.
type LiveState struct {
	Services   []string     `json:"services"` // services in the Desired State that was last loaded
	Containers []Container  `json:"containers"`
	Resources  []Resource   `json:"resources"`
	Health     HealthStatus `json:"health"`
}

// AppView is an Application plus its computed status, as returned by the API.
type AppView struct {
	Application
	SyncStatus    SyncStatus     `json:"syncStatus"`
	SyncMessage   string         `json:"syncMessage,omitempty"` // reason for Unknown / OutOfSync detail
	Health        HealthStatus   `json:"health"`
	TargetSHA     string         `json:"targetSHA,omitempty"`
	DeployedSHA   string         `json:"deployedSHA,omitempty"`
	LastOperation *SyncOperation `json:"lastOperation,omitempty"`
	ReconciledAt  time.Time      `json:"reconciledAt,omitzero"`
	PrunePending  []string       `json:"prunePending,omitempty"` // stopped undeclared services, and undeclared "network <name>" / "volume <name>"
	Hosts         []string       `json:"hosts,omitempty"`        // App of Apps: the Hosts its Child Applications deploy to, sorted
}

type DiffView struct {
	FromSHA  string        `json:"fromSHA"`
	ToSHA    string        `json:"toSHA"`
	Services []ServiceDiff `json:"services"`
	GitDiff  string        `json:"gitDiff"` // unified diff of the app path
	// Live is the app's compose files as written in the deployed Revision (the copy in the work dir),
	// Desired the same files at the target Revision; Live is empty before the first Sync.
	Live    string `json:"live"`
	Desired string `json:"desired"`
	// Warnings are problems in the Desired compose files that do not stop a Sync (e.g. secrets in plain text).
	Warnings []string `json:"warnings,omitempty"`
}

// Event is pushed to SSE subscribers.
type Event struct {
	Type string `json:"type"` // "app" (Data=AppView), "app-deleted" (Data=name), "op" (Data=SyncOperation), "progress" (Data=ProgressEvent)
	App  string `json:"app"`
	Data any    `json:"data"`
}

type ProgressEvent struct {
	OpID     int64  `json:"opID"`
	Resource string `json:"resource"` // e.g. "Container grafana-web-1", "Image nginx:1.27"
	Status   string `json:"status"`   // e.g. "Pulling", "Started"
	Text     string `json:"text,omitempty"`
}

// ---- Ports ----

type Store interface {
	ListApps(ctx context.Context) ([]Application, error)
	GetApp(ctx context.Context, name string) (Application, error)
	CreateApp(ctx context.Context, a Application) error
	UpdateApp(ctx context.Context, a Application) error
	DeleteApp(ctx context.Context, name string) error

	// CreateOp inserts op (Phase Running) and returns its ID. FinishOp sets phase/message/finishedAt
	// and trims history to the newest 20 operations per app.
	CreateOp(ctx context.Context, op SyncOperation) (int64, error)
	FinishOp(ctx context.Context, id int64, phase OpPhase, message string) error
	// SetOpPhase sets an unfinished op's phase and message (Waiting, or Running again).
	SetOpPhase(ctx context.Context, id int64, phase OpPhase, message string) error
	// SupersedeWaitingOps finishes the app's Waiting ops as Superseded by op by (Q81).
	SupersedeWaitingOps(ctx context.Context, app string, by int64) error
	GetOp(ctx context.Context, id int64) (SyncOperation, error)
	ListOps(ctx context.Context, app string) ([]SyncOperation, error) // newest first
	// LastSucceededOp returns the newest Succeeded op, or ErrNotFound.
	LastSucceededOp(ctx context.Context, app string) (SyncOperation, error)

	// Secrets in credentials are encrypted at rest and decrypted on read. Repository credentials are the
	// Source Server's (domain.Source).
	ListRegistryCredentials(ctx context.Context) ([]RegistryCredential, error)
	SaveRegistryCredential(ctx context.Context, c RegistryCredential) (int64, error)
	DeleteRegistryCredential(ctx context.Context, id int64) error

	// Hosts: secrets are sealed at rest; empty secret fields on save keep the old value.
	ListHosts(ctx context.Context) ([]Host, error)
	GetHost(ctx context.Context, name string) (Host, error)
	DeleteHost(ctx context.Context, name string) error

	// Enrollment (Q22, Q73): CreateInvite adds the Host by name if it's new and returns a single-use invite for it,
	// valid until expires; a new one replaces the Host's previous. ErrAlreadyExists once an Agent enrolled for it.
	CreateInvite(ctx context.Context, host string, expires time.Time) (invite string, err error)
	// Enroll uses up invite (ErrInvalid if unknown, used or expired at now): its Host gets the Agent's keys, waiting
	// for approval.
	Enroll(ctx context.Context, invite string, signingKey []byte, ageRecipient string, now time.Time) (Host, error)
	// HostBySigningKey finds the Host by its Agent's key, or by its previous key while a rotation isn't confirmed;
	// ErrNotFound for a key no Host has.
	HostBySigningKey(ctx context.Context, key []byte) (Host, error)
	// RotateSigningKey makes next the Host's key if old is its key now; old stays valid until ConfirmSigningKey.
	RotateSigningKey(ctx context.Context, host string, old, next []byte) error
	// ConfirmSigningKey records that the Agent authenticated with key: the current key ends a rotation, the previous
	// one undoes it (the Agent never switched).
	ConfirmSigningKey(ctx context.Context, host string, key []byte) error
	// SetAllowIPs replaces the Host's allowlist (CIDRs; none: any address).
	SetAllowIPs(ctx context.Context, host string, cidrs []string) error
	// ApplyAgentReport applies the Host's Agent's outbox entry seq once (M3-06): an entry at or below the last applied
	// seq changes nothing (applied false). A report on a Sync the Control Plane sent finishes that Sync Operation if it
	// is still unfinished; one the Agent ran itself becomes a finished Sync Operation, a rollback also marking the
	// Application rolled back with Auto-Sync off (Q25). Only Applications on that Host are touched.
	ApplyAgentReport(ctx context.Context, host string, seq uint64, r AgentReport) (applied bool, err error)
	// ApplyAgentRemoval applies an outbox entry seq saying app was removed: the Application, if on that Host and being
	// deleted, leaves the Control Plane (Q83). Once, like ApplyAgentReport.
	ApplyAgentRemoval(ctx context.Context, host string, seq uint64, app string) (applied bool, err error)
	// SetAppDeleting records that the Application is being deleted (Q83), how ("containers", "volumes" or "keep"),
	// until its Agent confirms; "" clears it.
	SetAppDeleting(ctx context.Context, name, how string) error
	// SaveAgentStatus keeps an Application's latest report from its Host's Agent; ErrNotFound unless the Application is
	// on that Host. AgentStatus is the latest, ErrNotFound before any.
	SaveAgentStatus(ctx context.Context, host, app string, st AgentStatus) error
	AgentStatus(ctx context.Context, app string) (AgentStatus, error)
	// SaveAgentDiff keeps the latest Diff an Application's Agent computed; AgentDiff is it, ErrNotFound before any.
	SaveAgentDiff(ctx context.Context, app string, d AgentDiff) error
	AgentDiff(ctx context.Context, app string) (AgentDiff, error)
	// AgentLeft records when the Host's Agent's connection ended (AgentInfo.LeftAt).
	AgentLeft(ctx context.Context, host string, at time.Time) error
	// RecordAgent keeps what the Host's Agent said about itself.
	RecordAgent(ctx context.Context, host string, info AgentInfo) error
	// LockHost locks the Host with the connections that shared its key; UnlockHost clears it.
	LockHost(ctx context.Context, host string, conns []AgentConnection) error
	UnlockHost(ctx context.Context, host string) error
	// ApproveHost approves the Host's key if fingerprint is the key's (ErrInvalid otherwise, or without a key).
	ApproveHost(ctx context.Context, host, fingerprint string) error

	ListTokens(ctx context.Context) ([]APIToken, error)
	CreateToken(ctx context.Context, name string) (APIToken, string, error) // returns plaintext once
	DeleteToken(ctx context.Context, id int64) error
	VerifyToken(ctx context.Context, plaintext string) (APIToken, error) // ErrNotFound if invalid; updates LastUsed

	// Admin user: single account "admin".
	AdminPasswordHash(ctx context.Context) (hash string, mustChange bool, err error) // ErrNotFound if not initialised
	SetAdminPassword(ctx context.Context, hash string, mustChange bool) error

	// UI sessions keyed by sha256 hex of the cookie value. CreateSession also purges expired sessions.
	CreateSession(ctx context.Context, tokenHash string, expires time.Time) error
	GetSession(ctx context.Context, tokenHash string) (expires time.Time, err error) // ErrNotFound if missing or expired
	ExtendSession(ctx context.Context, tokenHash string, expires time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteSessionsExcept(ctx context.Context, keepHash string) error

	Close() error
}

// Git reads Repositories. Credentials are looked up by the implementation via RepoCredentials.
// Source is the Source Server (Q5, Q12): the only part that reads Git. It renders Desired States with secrets
// still encrypted and serves their files by hash.
type Source interface {
	// Resolve fetches the Repository and resolves rev (branch, tag or SHA) to a commit SHA.
	Resolve(ctx context.Context, repoURL, rev string) (string, error)
	// Invalidate makes the next Resolve of repoURL fetch even within the fetch cooldown
	// (used for explicit Refresh, manual Sync and webhooks).
	Invalidate(repoURL string)
	// Render reads the files under spec.Path at spec.Revision and validates them. The same spec always renders
	// the same result.
	Render(ctx context.Context, spec RenderSpec) (Rendered, error)
	// Blob is the content of a rendered file, by its sha256.
	Blob(ctx context.Context, hash string) ([]byte, error)
	// Diff is a unified diff of path between two commits.
	Diff(ctx context.Context, repoURL, fromSHA, toSHA, path string) (string, error)
	// CheckAccess tells whether repoURL is readable, and with which credential, without cloning it.
	CheckAccess(ctx context.Context, repoURL string) (RepoAccess, error)

	// Repository credentials live only here (Q13, Q40): secrets go in, never out.
	ListCredentials(ctx context.Context) ([]RepoCredential, error)
	// SaveCredential creates (ID 0) or updates c; empty secret fields keep the old value.
	SaveCredential(ctx context.Context, c RepoCredential) (RepoCredential, error)
	DeleteCredential(ctx context.Context, id int64) error
}

type RenderSpec struct {
	RepoURL, Revision, Path string
	ComposeFiles, Profiles  []string
	Apps                    bool // an App of Apps: read its definitions instead of compose files
}

// Rendered is an Application's Desired State at a Revision.
type Rendered struct {
	Files []File // sorted by Path
	// Checksum is the sha256 of Files: it changes exactly when a file under the path does.
	Checksum string
	Apps     []Application // an App of Apps' definitions
	Warnings []string
	// Invalid is why the files can't be deployed (ErrInvalid); a Sync fails with it.
	Invalid error
}

// File is one file of a Rendered Desired State; its content is the blob named SHA256.
type File struct {
	Path   string      // slash-separated, relative to the Application's path
	Mode   fs.FileMode // 0644, 0755, or fs.ModeSymlink (content is the link target)
	SHA256 string
}

// Secrets decrypts Encrypted Secrets (*.sops.* files) in a directory.
type Secrets interface {
	// DecryptDir writes each "x.sops.ext" as "x.ext" (mode 0600) next to it, deletes "x.sops.ext", and returns
	// the written paths (also those written before an error).
	// identities are age secret keys (AGE-SECRET-KEY-1...), tried in order; only they are used.
	// A file none of them can decrypt fails with a *WrongKeyError.
	DecryptDir(dir string, identities []string) ([]string, error)
}

type SyncOptions struct {
	Prune bool
	// PruneVolumes (only with Prune) also deletes project volumes the project no longer declares and no container uses.
	PruneVolumes bool
	Registries   []RegistryCredential
	// Auth, when set, is asked for the credentials of the registries the project's images come from, before pulling
	// (an Agent asks its Control Plane, Q85); they are used with Registries
	Auth     func(ctx context.Context, registries []string) ([]RegistryCredential, error)
	Progress func(ProgressEvent) // may be nil
	// ForceRecreate recreates every container even if its config is unchanged
	// (after a restore, bind mounts must be re-attached to the restored files).
	ForceRecreate bool
}

// Engine runs Compose projects on a Host. dir is the Application's stable working directory
// holding its exported (and decrypted) files; the Compose project name is the Application name.
type Engine interface {
	// Validate loads the project and rejects unsupported things (e.g. build:).
	Validate(ctx context.Context, app Application, dir string) error
	// Diff compares the Desired State in dir against the Live State. When asDir is set, dir is a
	// staging copy and paths are evaluated as if the files lived in asDir (the app's working dir).
	Diff(ctx context.Context, app Application, dir, asDir string) ([]ServiceDiff, error)
	// Sync pulls images and brings the project up; with Prune it removes orphaned containers/networks
	// (and with PruneVolumes undeclared volumes), skipping networks and volumes a container still uses.
	Sync(ctx context.Context, app Application, dir string, opts SyncOptions) error
	// Remove deletes the project's containers and networks, and with volumes its named and anonymous
	// volumes (like `docker compose down -v`; never external volumes or host paths).
	Remove(ctx context.Context, app Application, volumes bool) error
	// Status returns the Live State, including Health per container and overall.
	Status(ctx context.Context, app Application) (LiveState, error)
	// ProjectExists reports whether a Compose project with this name has containers, and whether omegacd manages it.
	ProjectExists(ctx context.Context, name string) (exists, managed bool, err error)
	// Logs streams the logs (stdout+stderr, demultiplexed, with timestamps) of one of app's containers to w
	// until ctx ends or, if !follow, EOF. A container of another project is ErrNotFound.
	Logs(ctx context.Context, app Application, containerID string, follow bool, tail int, w io.Writer) error
	// ProjectFiles lists the env files, configs, secrets and bind mounts (inside dir) of the project in dir.
	ProjectFiles(ctx context.Context, app Application, dir string) ([]Resource, error)
	// Inspect describes a volume or network of the project.
	Inspect(ctx context.Context, app Application, kind, name string) (map[string]string, error)
	// Watch calls changed with the Compose project name whenever one of its containers changes state, and with an
	// empty project after reconnecting (events may have been missed: resync all of it). It blocks until ctx ends.
	Watch(ctx context.Context, changed func(project string)) error
	Close() error
}

// Controller is what the API drives.
type Controller interface {
	ListApps(ctx context.Context) ([]AppView, error)
	GetApp(ctx context.Context, name string) (AppView, error)
	CreateApp(ctx context.Context, a Application) (AppView, error)
	UpdateApp(ctx context.Context, a Application) (AppView, error)
	// DeleteApp deletes an Application; cascade also removes what it runs, volumes (with cascade) its volumes too.
	DeleteApp(ctx context.Context, name string, cascade, volumes bool) error
	// ForgetApp removes an Application from the Control Plane only, its containers left as they are, for one whose
	// Host is gone for good (Q92).
	ForgetApp(ctx context.Context, name string) error

	// Sync starts a Sync Operation to the app's target revision in the background and returns its ID.
	Sync(ctx context.Context, name string, req SyncRequest) (int64, error)
	// Rollback syncs to the revision of an earlier Succeeded op and turns Auto-Sync off.
	Rollback(ctx context.Context, name string, opID int64) (int64, error)
	// Refresh re-fetches Git and recomputes status now.
	Refresh(ctx context.Context, name string) (AppView, error)
	History(ctx context.Context, name string) ([]SyncOperation, error)
	Diff(ctx context.Context, name string) (DiffView, error)
	Resources(ctx context.Context, name string) (LiveState, error)
	// ResourceDetail describes one of the app's Resources; secret content is never returned.
	ResourceDetail(ctx context.Context, name, kind, resName string) (ResourceDetail, error)
	Logs(ctx context.Context, name, containerID string, follow bool, tail int, w io.Writer) error

	// RepoChanged is called by webhooks: refresh every app on repoURL (matched loosely: scheme, .git suffix, case).
	RepoChanged(ctx context.Context, repoURL string)
	Subscribe() (<-chan Event, func())

	ListHosts(ctx context.Context) ([]HostView, error)
	// DeleteHost refuses (ErrInvalid) while Applications use the Host.
	DeleteHost(ctx context.Context, name string) error
	// InviteHost adds a Host reached through an Agent, by name, and returns a single-use invite for it, valid an hour
	// (Q22, Q73); for such a Host no Agent has enrolled for yet, a new invite that replaces the last.
	InviteHost(ctx context.Context, name string) (invite string, expires time.Time, err error)
	// HostKeys is the public half of the Host's Host Key, from its Agent (Q108); none before an Agent enrolled.
	HostKeys(ctx context.Context, host string) ([]HostKey, error)
	// CheckRepo reports whether repoURL can be read (ErrInvalid if empty).
	CheckRepo(ctx context.Context, repoURL string) (RepoAccess, error)
}
