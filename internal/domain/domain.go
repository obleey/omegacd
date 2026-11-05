// Package domain holds the types and ports shared by every omegacd package.
// Terms follow GLOSSARY.md: Application, Host, Repository, Revision, Sync, ...
package domain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LocalHost is the built-in Host: the Docker engine omegacd talks to via DOCKER_HOST / the socket.
const LocalHost = "local"

// ErrLocalHostDisabled: OMEGACD_LOCAL_HOST=false, there is no local Docker engine (e.g. in Kubernetes).
var ErrLocalHostDisabled = fmt.Errorf("%w: the local Host is disabled (OMEGACD_LOCAL_HOST=false): choose a remote Host", ErrInvalid)

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
)

type Application struct {
	Name           string    `json:"name"`
	RepoURL        string    `json:"repoURL"`
	Path           string    `json:"path"`           // directory inside the repo, "" or "." for root
	TargetRevision string    `json:"targetRevision"` // branch, tag or commit SHA; default "main"
	ComposeFiles   []string  `json:"composeFiles"`   // relative to Path; empty: the first of DefaultComposeFiles found
	Profiles       []string  `json:"profiles"`
	Host           string    `json:"host"` // a Host name; default LocalHost
	AutoSync       bool      `json:"autoSync"`
	SelfHeal       bool      `json:"selfHeal"`
	Prune          bool      `json:"prune"`
	PruneVolumes   bool      `json:"pruneVolumes"`     // with Prune, also delete undeclared project volumes; false whenever Prune is
	Adopt          bool      `json:"adopt"`            // only honoured on create
	RolledBack     bool      `json:"rolledBack"`       // Auto-Sync was turned off by a Rollback; cleared when Auto-Sync is re-enabled
	Kind           string    `json:"kind"`             // KindCompose (default) or KindApps (an App of Apps)
	Parent         string    `json:"parent,omitempty"` // the App of Apps that owns this Child Application
	NotInGit       bool      `json:"notInGit"`         // a Child Application its parent no longer declares (kept because Prune is off)
	CreatedAt      time.Time `json:"createdAt"`
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

// MarkerFile marks a folder OmegaCD created in a work dir and holds the name of the Application it belongs to.
// A Sync never writes into a folder with content that lacks this Application's marker.
const MarkerFile = ".omegacd"

// LegacyDir held every Application's files before Dir (<work dir>/apps/<name>); a Sync removes the old folder.
const LegacyDir = "apps"

// Host is a Docker engine Applications deploy to. LocalHost is built in and not stored.
type Host struct {
	Name          string    `json:"name"`
	URL           string    `json:"url"`     // ssh://user@server[:port] or tcp://server:2376
	WorkDir       string    `json:"workDir"` // Application files are copied here on the host; default /var/lib/omegacd (persistent across reboots)
	SSHPrivateKey string    `json:"-"`
	TLSCA         string    `json:"-"` // PEM; TLS is used when TLSCert and TLSKey are set
	TLSCert       string    `json:"-"`
	TLSKey        string    `json:"-"`
	HasSSHKey     bool      `json:"hasSSHKey"`
	HasTLS        bool      `json:"hasTLS"`
	CreatedAt     time.Time `json:"createdAt"`
}

// HostStatus is the result of contacting a Host's Docker engine.
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
	// KeyRecipient is the public key of the Host's current Host Key (the Master Key for LocalHost), "" if it has none.
	KeyRecipient string `json:"keyRecipient"`
}

// HostKey is an age X25519 key pair Encrypted Secrets for one Host are decrypted with.
// A Host has at most one Current key; the others are previous keys kept for decryption during a rotation.
// PrivateKey (AGE-SECRET-KEY-1...) is sealed at rest and never leaves the control plane.
type HostKey struct {
	Host       string    `json:"-"`
	Recipient  string    `json:"recipient"` // age1...
	PrivateKey string    `json:"-"`
	Current    bool      `json:"current"`
	Master     bool      `json:"master,omitempty"` // the Master Key, which is LocalHost's Host Key
	CreatedAt  time.Time `json:"createdAt,omitzero"`
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
	GetOp(ctx context.Context, id int64) (SyncOperation, error)
	ListOps(ctx context.Context, app string) ([]SyncOperation, error) // newest first
	// LastSucceededOp returns the newest Succeeded op, or ErrNotFound.
	LastSucceededOp(ctx context.Context, app string) (SyncOperation, error)

	// Secrets in credentials are encrypted at rest and decrypted on read.
	ListRepoCredentials(ctx context.Context) ([]RepoCredential, error)
	SaveRepoCredential(ctx context.Context, c RepoCredential) (int64, error) // ID 0 = create; empty secret fields keep the old value
	DeleteRepoCredential(ctx context.Context, id int64) error
	ListRegistryCredentials(ctx context.Context) ([]RegistryCredential, error)
	SaveRegistryCredential(ctx context.Context, c RegistryCredential) (int64, error)
	DeleteRegistryCredential(ctx context.Context, id int64) error

	// Hosts: secrets are sealed at rest; empty secret fields on save keep the old value.
	ListHosts(ctx context.Context) ([]Host, error)
	GetHost(ctx context.Context, name string) (Host, error)
	SaveHost(ctx context.Context, h Host) error        // create or update by Name
	DeleteHost(ctx context.Context, name string) error // also deletes its Host Keys

	// Host Keys: PrivateKey is sealed at rest. ListHostKeys returns the current key first, then the previous ones, newest first.
	ListHostKeys(ctx context.Context, host string) ([]HostKey, error)
	// AddHostKey stores k as the Host's current key; the old current key becomes previous. ErrAlreadyExists if the Host has it already.
	AddHostKey(ctx context.Context, k HostKey) error
	DeleteHostKey(ctx context.Context, host, recipient string) error

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
type Git interface {
	// Resolve fetches the Repository and resolves rev (branch, tag or SHA) to a commit SHA.
	Resolve(ctx context.Context, repoURL, rev string) (string, error)
	// Invalidate makes the next Resolve of repoURL fetch even within the fetch cooldown
	// (used for explicit Refresh, manual Sync and webhooks).
	Invalidate(repoURL string)
	// TreeHash is the git tree hash of path at sha ("" if the path does not exist).
	TreeHash(ctx context.Context, repoURL, sha, path string) (string, error)
	// Export writes the files under path at sha into dest, overwriting files in place (same inode)
	// and deleting files in dest that are not in the tree.
	Export(ctx context.Context, repoURL, sha, path, dest string) error
	// Diff is a unified diff of path between two commits.
	Diff(ctx context.Context, repoURL, fromSHA, toSHA, path string) (string, error)
	// CheckAccess tells whether repoURL is readable, and with which credential, without cloning it.
	CheckAccess(ctx context.Context, repoURL string) (RepoAccess, error)
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
	Progress     func(ProgressEvent) // may be nil
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
	// ProjectExists reports whether a Compose project with this name has containers on host, and whether omegacd manages it.
	ProjectExists(ctx context.Context, host, name string) (exists, managed bool, err error)
	// HostStatus contacts the host's Docker engine.
	HostStatus(ctx context.Context, host string) HostStatus
	// HostChanged drops cached connections after a Host is edited or deleted.
	HostChanged(host string)
	// Logs streams the logs (stdout+stderr, demultiplexed, with timestamps) of one of app's containers to w
	// until ctx ends or, if !follow, EOF. A container of another project is ErrNotFound.
	Logs(ctx context.Context, app Application, containerID string, follow bool, tail int, w io.Writer) error
	// ProjectFiles lists the env files, configs, secrets and bind mounts (inside dir) of the project in dir.
	ProjectFiles(ctx context.Context, app Application, dir string) ([]Resource, error)
	// Inspect describes a volume or network of the project.
	Inspect(ctx context.Context, app Application, kind, name string) (map[string]string, error)
	// Watch calls changed with the Host and Compose project name whenever a container on any Host changes state,
	// and with an empty project after reconnecting to a Host (events may have been missed: resync all of it).
	// It blocks, reconnecting as needed, until ctx ends.
	Watch(ctx context.Context, changed func(host, project string)) error
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

	// ListHosts returns the built-in LocalHost first, then the stored Hosts.
	ListHosts(ctx context.Context) ([]HostView, error)
	// SaveHost creates (create=true, ErrAlreadyExists if taken) or updates a Host; LocalHost is reserved.
	SaveHost(ctx context.Context, h Host, create bool) (HostView, error)
	// DeleteHost refuses (ErrInvalid) while Applications use the Host.
	DeleteHost(ctx context.Context, name string) error
	TestHost(ctx context.Context, name string) (HostStatus, error)
	// HostKeys lists a Host's keys without private keys (LocalHost: just the Master Key).
	HostKeys(ctx context.Context, host string) ([]HostKey, error)
	// GenerateHostKey and ImportHostKey (an AGE-SECRET-KEY-1... string) add a new current key; the old one becomes previous.
	GenerateHostKey(ctx context.Context, host string) (HostKey, error)
	ImportHostKey(ctx context.Context, host, privateKey string) (HostKey, error)
	// RemoveHostKey removes a previous key (ErrInvalid for the current one).
	RemoveHostKey(ctx context.Context, host, recipient string) error
	// CheckRepo reports whether repoURL can be read (ErrInvalid if empty).
	CheckRepo(ctx context.Context, repoURL string) (RepoAccess, error)
}
