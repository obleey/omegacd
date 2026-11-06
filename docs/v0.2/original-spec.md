> **Superseded.** This is the input to the v0.2 design review, kept for reference. The decided design is
> [architecture.md](architecture.md); where they differ, architecture.md and [decisions.md](decisions.md) win
> (e.g. no separate repo-server download path, no access/refresh tokens, Source Server instead of repo server,
> in-place workspace sync instead of wipe-and-copy).

# OmegaCD: Distributed GitOps Architecture Specification
## Target Architecture: Pull-Based Agent Mode with Embedded Components

---

## 1. Network Boundary and Traffic Flow

All long-lived communication channels are initiated **outbound from the Agent to the Central Server** using WebSockets over standard HTTP/2 (TLS Port 443). This ensures the server remains protected behind private networks, while the edge agents can operate behind strict firewalls or NATs without needing exposed incoming ports.

```
[ WORKLOAD NODE / EDGE HOST ]                                     [ OPS HUB / SERVER ]
+-----------------------------+                               +-------------------------+

|  +-----------------------+  |                               |  +-------------------+  |
|  | Docker Engine Socket  |  |                               |  |    Web UI / API   |  |
|  | (unix://...sock)      |  |                               |  |  (omegacd server) |  |
|  +-----------^-----------+  |                               |  +---------^---------+  |
|              |              |                               |            |            |
|  +-----------v-----------+  |    1. WebSockets (Port 443)   |  +---------v---------+  |
|  |     OmegaCD Agent     |---------------------------------->|   Central Engine    |  |
|  |    (omegacd agent)    |<----------------------------------|   & Token Router    |  |
|  +-----------┬-----------+  |    2. Encrypted Signals       |  +---------┬---------+  |
|              |              |                               |            |            |
|              |              |    3. HTTPS GET (Port 443)    |  +---------v---------+  |
|              +---------------------------------------------->|  Internal Repo-Svc  |  |
|                             |       Stream Tarball payload  |  | (Compiles Git/Hash) |  |
|                             |                               |  +-------------------+  |
+-----------------------------+                               +-------------------------+
```

---

## 2. CLI Routing & Architecture Footprint

To maintain OmegaCD's core philosophy of being a lean, lightweight tool, all personas (Server, Repo Server, and Agent) are compiled into a **single, unified Go binary**. Users switch modes via minimalist CLI flags.

```go
package main

import (
	"log"
	"os"
	"strings"
)

func main() {
	cmd := "server"
	if len(os.Args) > 1 {
		cmd = strings.ToLower(os.Args)
	}

	switch cmd {
	case "server":
		// Starts HTTP UI, API Engine, and boots the local Repo-Server goroutine
		RunCentralServer()
	case "repo-server":
		// Starts an isolated manifest compilation daemon
		RunIsolatedRepoServer()
	case "agent":
		// Starts the local edge pull loop and Docker control block
		RunEdgeAgent()
	default:
		// Routes cleanly into fallback Client commands (app sync, history, etc.)
		RunClientCLI()
	}
}
```

---

## 3. SQLite Database Schema Configuration

To track distributed edge agents, active token lifecycles, and single-use invites securely, execute the following schema configuration:

```sql
-- Core agent registration tracking table
CREATE TABLE agent_hosts (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    public_key TEXT NOT NULL,          -- Agent's native public age identity key
    status TEXT NOT NULL DEFAULT 'offline', -- 'online', 'offline', 'enrolling'
    last_seen_at DATETIME,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Access and Refresh token lifecycle isolation map
CREATE TABLE agent_tokens (
    id TEXT PRIMARY KEY,
    agent_id TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,   -- SHA-256 hash of token value string
    token_type TEXT NOT NULL,          -- 'access' (1h) or 'refresh' (30d)
    expires_at DATETIME NOT NULL,
    grace_period_until DATETIME,       -- Avoids network split orphaned nodes during rotation
    FOREIGN KEY(agent_id) REFERENCES agent_hosts(id) ON DELETE CASCADE
);

-- Single-use cryptographically random token invitations
CREATE TABLE agent_invites (
    code_hash TEXT PRIMARY KEY,        -- SHA-256 hash of plaintext invitation string
    generated_by TEXT NOT NULL,
    expires_at DATETIME NOT NULL
);
```

---

## 4. Cache & Workload Storage Hierarchy Layout

To avoid breaking relative bind-mount paths inside Docker Compose (e.g., `./nginx.conf:/etc/nginx/nginx.conf`), the Agent manages cache revisions inside an offline vault and uses an **Atomic Directory Copy** loop to place files into a completely static live deployment anchor point.

```text
/var/lib/omegacd-agent/
├── tokens/
│   ├── access.token                    <-- Extinguishes every 1 hour (Valid for HTTP payload GET requests)
│   └── refresh.token                   <-- Rotates every 45 mins; handles recoveries (Valid for 30 days)
├── keys/
│   └── agent.age                       <-- Hard cryptographic identity generated locally during initial launch
├── cache/
│   └── apps/
│       └── [app-id]/
│           └── revisions/              <-- Vault storage for the most recent 3 verified deployments
│               ├── revision-abc1234/
│               │   ├── .omegacd-manifest.json
│               │   ├── compose.yaml
│               │   └── secret.sops.env
│               └── revision-def5678/
└── active/
    └── [app-id]/
        └── workspace/                  <-- STATIC deployment anchor point. Wiped and overwritten atomically.
            ├── .omegacd-manifest.json
            ├── compose.yaml
            └── secret.env              <-- Decrypted plaintext materialized strictly inside memory-backed runtimes
```

---

## 5. Core Implementation Engine Logic

### 5.1 Atomic Directory Management (`internal/agent/cache.go`)

```go
package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Manifest struct {
	TargetRevision string            `json:"target_revision"`
	Files          map[string]string `json:"files"`
}

// AtomicRollbackCache copies cache content directly into the static live directory
func AtomicRollbackCache(appID, revisionID, agentBaseDir string) error {
	activeWorkspace := filepath.Join(agentBaseDir, "active", appID, "workspace")
	cacheRevisionPath := filepath.Join(agentBaseDir, "cache", "apps", appID, "revisions", "revision-"+revisionID)

	// Validate target cache exists before destructive operations
	if _, err := os.Stat(cacheRevisionPath); os.IsNotExist(err) {
		return fmt.Errorf("rollback failed: targeted cache checkpoint %s missing", revisionID)
	}

	// 1. Wipe out active workspace folder to avoid configuration blending
	if err := os.RemoveAll(activeWorkspace); err != nil {
		return fmt.Errorf("failed to clear active workspace: %w", err)
	}
	if err := os.MkdirAll(activeWorkspace, 0700); err != nil {
		return fmt.Errorf("failed to re-create active workspace target: %w", err)
	}

	// 2. Perform deep disk file transfer copy from read-only cache vault
	err := filepath.Walk(cacheRevisionPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cacheRevisionPath, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		targetPath := filepath.Join(activeWorkspace, rel)
		if info.IsDir() {
			return os.MkdirAll(targetPath, info.Mode())
		}
		return copyFile(path, targetPath, info.Mode())
	})
	if err != nil {
		return fmt.Errorf("cache extraction swap failed: %w", err)
	}

	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
```

### 5.2 Server WebSocket Signalling & Heartbeat (`internal/server/websocket.go`)

```go
package server

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type CentralHub struct {
	ActiveAgents map[string]*websocket.Conn
	mu           sync.RWMutex
}

func NewCentralHub() *CentralHub {
	return &CentralHub{ActiveAgents: make(map[string]*websocket.Conn)}
}

func (h *CentralHub) ServeAgentStream(hubWw http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(hubWw, r, nil)
	if err != nil {
		log.Printf("WebSocket connection initiation drop: %v", err)
		return
	}
	defer conn.Close()

	agentID := r.Header.Get("X-Agent-ID")

	h.mu.Lock()
	h.ActiveAgents[agentID] = conn
	h.mu.Unlock()

	log.Printf("Agent node connection established: %s", agentID)

	go h.monitorHeartbeat(agentID, conn)

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Agent connection closed or corrupted: %s, cleanup triggered", agentID)
			h.mu.Lock()
			delete(h.ActiveAgents, agentID)
			h.mu.Unlock()
			UpdateAgentStatusInDB(agentID, "offline")
			break
		}
	}
}

func (h *CentralHub) monitorHeartbeat(agentID string, conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		h.mu.RLock()
		_, exists := h.ActiveAgents[agentID]
		h.mu.RUnlock()
		if !exists {
			return
		}

		err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second))
		if err != nil {
			log.Printf("Heartbeat lost for target agent %s, disconnecting channel", agentID)
			conn.Close()
			return
		}
	}
}

func UpdateAgentStatusInDB(id, state string) {}
```

### 5.3 Agent Out-of-Band Payload Pull (`internal/agent/pull.go`)

```go
package agent

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

type SignalMessage struct {
	Event         string `json:"event"`
	ApplicationID string `json:"application_id"`
	TargetVersion string `json:"target_revision"`
}

func FetchAndProcessPayload(signal SignalMessage, serverURL, accessToken, baseDir string) error {
	payloadURL := fmt.Sprintf("%s/api/agents/v1/payloads/%s", serverURL, signal.ApplicationID)
	
	req, err := http.NewRequest("GET", payloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("network extraction transaction dropped: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("payload server rejected request: status %d", resp.StatusCode)
	}

	stagingDir := filepath.Join(baseDir, "cache", "apps", signal.ApplicationID, "revisions", "revision-"+signal.TargetVersion)
	if err := os.MkdirAll(stagingDir, 0700); err != nil {
		return err
	}

	targetTarFile := filepath.Join(stagingDir, "payload.tar.gz")
	out, err := os.Create(targetTarFile)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return err
	}

	return nil
}
```

---

## 6. Implementation Master Plan

```
[Milestone 1] ───────────────► [Milestone 2] ───────────────► [Milestone 3] ───────────────► [Milestone 4]
Command Split & Payload API    Zero-Trust Enrollment Engine  WebSocket Signaling Plane     Local Offline Cache Loop
```

### Milestone 1: Command Structure Split & API Payload Endpoints
* Restructure the `main.go` entry point to evaluate `server`, `repo-server`, and `agent` sub-commands.
* Package repository fetching operations (`internal/gitsrc`) into an isolated `reposerver` package scope.
* Implement the `/api/agents/v1/payloads/{app_id}` endpoint to compress target configs and embed `.omegacd-manifest.json` dynamically.

### Milestone 2: Registration Handshakes & Token Store
* Initialize the SQLite data layers tracking `agent_hosts`, `agent_tokens`, and `agent_invites`.
* Code SHA-256 token verification handlers into your server's request interceptor middleware.
* Build the UI invitation workflow hook that exposes single-use setup tokens to operators.

### Milestone 3: Real-Time Signal Channels & Active Token Rotation
* Establish the persistent WebSockets channel at `/api/agents/v1/stream` with built-in 30s heartbeats.
* Implement the agent-side ticker loop that proactively cycles credentials across the WebSocket every 45 minutes.
* Write token-pair recovery workflows to gracefully handle re-authentication via the 30-day `Refresh Token` during unexpected long outages.

### Milestone 4: Local Storage Cache & Local Recovery Loop
* Implement the atomic workspace directory wipe-and-copy block inside the agent routine.
* Code an automatic retention cleaner that limits local app revision data stores to the most recent 3 verified copies.
* Build the standalone `omegacd agent rollback [revision-id]` CLI pathway to allow air-gapped rollbacks directly on production boxes.
