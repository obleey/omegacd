package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/importer"
	"github.com/obleey/omegacd/internal/source"
	"github.com/obleey/omegacd/internal/version"
)

func (s *server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Controller.ListApps(r.Context())
	respond(w, http.StatusOK, apps, err)
}

func (s *server) createApp(w http.ResponseWriter, r *http.Request) {
	var a domain.Application
	if err := decode(w, r, &a); err != nil {
		writeError(w, err)
		return
	}
	v, err := s.Controller.CreateApp(r.Context(), a)
	respond(w, http.StatusCreated, v, err)
}

func (s *server) getApp(w http.ResponseWriter, r *http.Request) {
	v, err := s.Controller.GetApp(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, v, err)
}

func (s *server) updateApp(w http.ResponseWriter, r *http.Request) {
	var a domain.Application
	if err := decode(w, r, &a); err != nil {
		writeError(w, err)
		return
	}
	if a.Name != "" && a.Name != r.PathValue("name") {
		writeError(w, fmt.Errorf("%w: name cannot be changed", domain.ErrInvalid))
		return
	}
	a.Name = r.PathValue("name")
	v, err := s.Controller.UpdateApp(r.Context(), a)
	respond(w, http.StatusOK, v, err)
}

// deleteApp deletes an Application; ?forget=true only from the Control Plane (Q92). A delete its offline Host's Agent
// must do waits for it: 202 (Q83).
func (s *server) deleteApp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("forget") == "true" {
		respond(w, http.StatusNoContent, nil, s.Controller.ForgetApp(r.Context(), r.PathValue("name")))
		return
	}
	err := s.Controller.DeleteApp(r.Context(), r.PathValue("name"), q.Get("cascade") == "true", q.Get("volumes") == "true")
	if errors.Is(err, domain.ErrWaitingForHost) {
		writeJSON(w, http.StatusAccepted, map[string]string{"message": err.Error()})
		return
	}
	respond(w, http.StatusNoContent, nil, err)
}

func (s *server) syncApp(w http.ResponseWriter, r *http.Request) {
	var req domain.SyncRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeStatus(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}
	id, err := s.Controller.Sync(r.Context(), r.PathValue("name"), req)
	respond(w, http.StatusAccepted, map[string]int64{"opID": id}, err)
}

func (s *server) rollbackApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OpID int64 `json:"opID"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	id, err := s.Controller.Rollback(r.Context(), r.PathValue("name"), req.OpID)
	respond(w, http.StatusAccepted, map[string]int64{"opID": id}, err)
}

// cancelSync cancels a Sync waiting for its Host (Q154).
func (s *server) cancelSync(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Controller.CancelSync(r.Context(), r.PathValue("name"), id)
	}
	respond(w, http.StatusNoContent, nil, err)
}

func (s *server) refreshApp(w http.ResponseWriter, r *http.Request) {
	v, err := s.Controller.Refresh(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, v, err)
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	ops, err := s.Controller.History(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, ops, err)
}

func (s *server) diff(w http.ResponseWriter, r *http.Request) {
	d, err := s.Controller.Diff(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, d, err)
}

func (s *server) resources(w http.ResponseWriter, r *http.Request) {
	l, err := s.Controller.Resources(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, l, err)
}

func (s *server) resource(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, err := s.Controller.ResourceDetail(r.Context(), r.PathValue("name"), q.Get("kind"), q.Get("name"))
	respond(w, http.StatusOK, d, err)
}

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	v, err := s.Controller.GetApp(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	a := v.Application
	out, err := appdef.Marshal(a)
	if err != nil { // coverage: unreachable, a definition is strings, lists and booleans: it always encodes as YAML
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", a.Name+".yaml"))
	w.Write(out)
}

func respond(w http.ResponseWriter, status int, v any, err error) {
	switch {
	case err != nil:
		writeError(w, err)
	case status == http.StatusNoContent:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, v)
	}
}

// sse prepares a long-lived event stream response.
func sse(w http.ResponseWriter) *http.ResponseController {
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return rc
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	ch, unsubscribe := s.Controller.Subscribe()
	defer unsubscribe()
	rc := sse(w)
	io.WriteString(w, ": connected\n\n")
	rc.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
		case ev, ok := <-ch:
			if !ok { // coverage: unreachable, only this handler's own unsubscribe closes it, when it returns
				return
			}
			data, err := json.Marshal(ev)
			if err != nil { // coverage: unreachable, events hold views, operations and progress: plain structs
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data)
		}
		if rc.Flush() != nil {
			return
		}
	}
}

func (s *server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tail, err := strconv.Atoi(q.Get("tail"))
	if err != nil {
		tail = 200
	}
	follow := q.Get("follow") == "1" || q.Get("follow") == "true"
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() {
		pw.CloseWithError(s.Controller.Logs(ctx, r.PathValue("name"), r.PathValue("container"), follow, tail, pw))
	}()

	var rc *http.ResponseController
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if rc == nil {
			rc = sse(w)
		}
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", strings.TrimRight(sc.Text(), "\r"))
		if rc.Flush() != nil {
			return
		}
	}
	err = sc.Err()
	if rc == nil {
		if err != nil && ctx.Err() == nil {
			writeError(w, err) // nothing streamed yet, so a proper status code is still possible
			return
		}
		rc = sse(w)
	} else if err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
	}
	io.WriteString(w, "event: end\ndata: \n\n")
	rc.Flush()
}

func (s *server) webhook(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider != "github" && provider != "gitlab" && provider != "gitea" && provider != "generic" {
		writeError(w, fmt.Errorf("webhook provider %q: %w", provider, domain.ErrNotFound))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		writeError(w, fmt.Errorf("%w: %v", domain.ErrInvalid, err))
		return
	}
	if s.WebhookParse == nil {
		writeStatus(w, http.StatusNotImplemented, "webhooks are not configured")
		return
	}
	urls, err := s.WebhookParse(provider, r.Header, body, s.WebhookSecret)
	if errors.Is(err, domain.ErrInvalid) {
		writeError(w, err)
		return
	}
	if err != nil {
		writeStatus(w, http.StatusUnauthorized, err.Error())
		return
	}
	for _, u := range urls {
		go s.Controller.RepoChanged(context.Background(), u)
	}
	w.WriteHeader(http.StatusAccepted)
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: id %q", domain.ErrInvalid, r.PathValue("id"))
	}
	return id, nil
}

// optionalID is 0 for POST (create) and the path id for PUT (update).
func optionalID(r *http.Request) (int64, error) {
	if r.PathValue("id") == "" {
		return 0, nil
	}
	return pathID(r)
}

// repoCredView is a RepoCredential plus the Applications whose Repository it serves.
type repoCredView struct {
	domain.RepoCredential
	UsedBy []string `json:"usedBy"`
}

func (s *server) listRepoCreds(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Source.ListCredentials(r.Context())
	var apps []domain.AppView
	if err == nil {
		apps, err = s.Controller.ListApps(r.Context())
	}
	out := make([]repoCredView, len(cs))
	for i, c := range cs {
		out[i] = repoCredView{RepoCredential: c, UsedBy: []string{}}
	}
	for _, a := range apps {
		if c := source.MatchCredential(cs, a.RepoURL); c != nil {
			i := slices.IndexFunc(cs, func(x domain.RepoCredential) bool { return x.ID == c.ID })
			out[i].UsedBy = append(out[i].UsedBy, a.Name)
		}
	}
	for _, v := range out {
		slices.Sort(v.UsedBy)
	}
	respond(w, http.StatusOK, out, err)
}

func (s *server) checkRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RepoURL string `json:"repoURL"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	v, err := s.Controller.CheckRepo(r.Context(), req.RepoURL)
	respond(w, http.StatusOK, v, err)
}

func (s *server) saveRepoCred(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLPrefix     string `json:"urlPrefix"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		SSHPrivateKey string `json:"sshPrivateKey"`
	}
	id, err := optionalID(r)
	if err == nil {
		err = decode(w, r, &req)
	}
	if err == nil && req.URLPrefix == "" {
		err = fmt.Errorf("%w: urlPrefix is required", domain.ErrInvalid)
	}
	var saved domain.RepoCredential
	if err == nil {
		saved, err = s.Source.SaveCredential(r.Context(), domain.RepoCredential{
			ID: id, URLPrefix: req.URLPrefix, Username: req.Username, Password: req.Password, SSHPrivateKey: req.SSHPrivateKey,
		})
	}
	respond(w, saveStatus(r), saved, err)
}

func (s *server) deleteRepoCred(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Source.DeleteCredential(r.Context(), id)
	}
	respond(w, http.StatusNoContent, nil, err)
}

// sourceStatus is Settings → Source Server (Q156, Q202, Q203): where it runs, whether it answers and how fast, its
// version (warned when it doesn't fit the Control Plane's) and its caches. Unreachable is an answer, not an error.
func (s *server) sourceStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Mode      string `json:"mode"`
		URL       string `json:"url,omitempty"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
		LatencyMs int64  `json:"latencyMs"`
		Warning   string `json:"warning,omitempty"`
		domain.SourceStatus
	}{Mode: "in-process", URL: s.SourceURL}
	if s.SourceURL != "" {
		out.Mode = "standalone"
	}
	start := time.Now()
	st, err := s.Source.Status(r.Context())
	out.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		s.Log.WarnContext(r.Context(), "source server status", "err", err)
		out.Error = err.Error()
	} else {
		out.Reachable, out.SourceStatus = true, st
		cp := version.String()
		if st.Version != "" && !version.Compatible(cp, st.Version) {
			out.Warning = fmt.Sprintf("Source Server %s doesn't fit OmegaCD %s (the same minor version or one behind): upgrade it", st.Version, cp)
		}
	}
	respond(w, http.StatusOK, out, nil)
}

func (s *server) listHosts(w http.ResponseWriter, r *http.Request) {
	hs, err := s.Controller.ListHosts(r.Context())
	respond(w, http.StatusOK, hs, err)
}

// inviteHost adds a Host by name, or gives a Host no Agent has enrolled for yet a new invite (Q22, Q73).
func (s *server) inviteHost(w http.ResponseWriter, r *http.Request) {
	invite, expires, err := s.Controller.InviteHost(r.Context(), r.PathValue("name"))
	respond(w, http.StatusCreated, map[string]any{"invite": invite, "expiresAt": expires}, err)
}

// approveHost approves the Host's Agent; the request names the fingerprint the admin compared (Q28).
func (s *server) approveHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if s.noAgents(w) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.Agents.Approve(r.Context(), r.PathValue("name"), req.Fingerprint))
}

// rotateHost has the Host's connected Agent make a new signing key (Q76).
func (s *server) rotateHost(w http.ResponseWriter, r *http.Request) {
	if s.noAgents(w) {
		return
	}
	fingerprint, err := s.Agents.Rotate(r.Context(), r.PathValue("name"))
	if errors.Is(err, agents.ErrOffline) || errors.Is(err, agents.ErrNotApproved) {
		err = fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	respond(w, http.StatusOK, map[string]string{"fingerprint": fingerprint}, err)
}

// importHost has the Host's Agent import the Compose stacks running there (M6-05): the body is importer.Request, the
// answer every stack's importer.Result with its files, secrets encrypted on the Host (Q230).
func (s *server) importHost(w http.ResponseWriter, r *http.Request) {
	var req importer.Request
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if s.noAgents(w) {
		return
	}
	results, err := s.Agents.Import(r.Context(), r.PathValue("name"), req)
	if errors.Is(err, agents.ErrOffline) || errors.Is(err, agents.ErrNotApproved) {
		err = fmt.Errorf("%w: %v", domain.ErrInvalid, err)
	}
	respond(w, http.StatusOK, results, err)
}

// unlockHost lets a Host locked by two connections with its key connect again (Q75).
func (s *server) unlockHost(w http.ResponseWriter, r *http.Request) {
	if s.noAgents(w) {
		return
	}
	respond(w, http.StatusNoContent, nil, s.Agents.Unlock(r.Context(), r.PathValue("name")))
}

// setHostAllowIPs replaces the addresses the Host's Agent may connect from (Q28); an empty list allows any.
func (s *server) setHostAllowIPs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AllowIPs []string `json:"allowIPs"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if s.noAgents(w) {
		return
	}
	list, err := s.Agents.SetAllowlist(r.Context(), r.PathValue("name"), req.AllowIPs)
	respond(w, http.StatusOK, map[string][]string{"allowIPs": list}, err)
}

func (s *server) noAgents(w http.ResponseWriter) bool {
	if s.Agents == nil {
		writeError(w, fmt.Errorf("%w: this server accepts no Agents", domain.ErrInvalid))
	}
	return s.Agents == nil
}

// deleteHost also revokes its Agent's key: its live connection is closed, and the key known nowhere (Q22).
func (s *server) deleteHost(w http.ResponseWriter, r *http.Request) {
	err := s.Controller.DeleteHost(r.Context(), r.PathValue("name"))
	if err == nil && s.Agents != nil {
		s.Agents.Disconnect(r.PathValue("name"))
	}
	respond(w, http.StatusNoContent, nil, err)
}

// renameHost renames a Host no Application uses (Q162); its Agent's connection is closed, as the hub knows it by
// name, and the Agent reconnects under the new one.
func (s *server) renameHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	err := s.Controller.RenameHost(r.Context(), r.PathValue("name"), req.Name)
	if err == nil && s.Agents != nil {
		s.Agents.Disconnect(r.PathValue("name"))
	}
	respond(w, http.StatusNoContent, nil, err)
}

// hostKeys is the public half of the Host's Host Key, from its Agent (Q108).
func (s *server) hostKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Controller.HostKeys(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, keys, err)
}

func (s *server) listRegistryCreds(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.ListRegistryCredentials(r.Context())
	respond(w, http.StatusOK, cs, err)
}

func (s *server) saveRegistryCred(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Registry string `json:"registry"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	id, err := optionalID(r)
	if err == nil {
		err = decode(w, r, &req)
	}
	if err == nil && req.Registry == "" {
		err = fmt.Errorf("%w: registry is required", domain.ErrInvalid)
	}
	// a registry is a host like ghcr.io; a URL here is almost always a Git credential in the wrong form
	if err == nil && (strings.Contains(req.Registry, "://") || strings.Contains(req.Registry, "/")) {
		err = fmt.Errorf("%w: registry must be a host like ghcr.io or registry.example.com:5000 — to read a Git repository, add it under Repository credentials", domain.ErrInvalid)
	}
	if err == nil {
		id, err = s.Store.SaveRegistryCredential(r.Context(), domain.RegistryCredential{
			ID: id, Registry: req.Registry, Username: req.Username, Password: req.Password,
		})
	}
	var saved domain.RegistryCredential
	if err == nil {
		var cs []domain.RegistryCredential
		cs, err = s.Store.ListRegistryCredentials(r.Context())
		saved, err = find(cs, err, id, func(c domain.RegistryCredential) int64 { return c.ID })
	}
	respond(w, saveStatus(r), saved, err)
}

func (s *server) deleteRegistryCred(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.DeleteRegistryCredential(r.Context(), id)
	}
	respond(w, http.StatusNoContent, nil, err)
}

// listEncryptionKeys is the people's public keys Hosts' secrets are encrypted for (Q189, Q199): what secrets init
// and rekey read.
func (s *server) listEncryptionKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListEncryptionKeys(r.Context())
	respond(w, http.StatusOK, keys, err)
}

// saveEncryptionKey adds the key named in the path, or changes it (Q292).
func (s *server) saveEncryptionKey(w http.ResponseWriter, r *http.Request) {
	k := domain.EncryptionKey{Name: strings.TrimSpace(r.PathValue("name"))}
	var req struct {
		Recipient string   `json:"recipient"`
		AllHosts  bool     `json:"allHosts"`
		Hosts     []string `json:"hosts"`
	}
	err := decode(w, r, &req)
	if err == nil && k.Name == "" {
		err = fmt.Errorf("%w: a key needs a name, such as whose laptop it is", domain.ErrInvalid)
	}
	if err == nil {
		k.Recipient, k.AllHosts, k.Hosts = strings.TrimSpace(req.Recipient), req.AllHosts, req.Hosts
		if _, perr := age.ParseX25519Recipient(k.Recipient); perr != nil {
			err = fmt.Errorf("%w: that is not an age public key (age1…): send the public half, never the private key", domain.ErrInvalid) // not echoed: it may be the private one
		}
	}
	if err == nil && k.AllHosts == (len(k.Hosts) > 0) {
		err = fmt.Errorf("%w: a key covers all Hosts or the ones listed: one of them", domain.ErrInvalid)
	}
	if err == nil {
		err = s.Store.SaveEncryptionKey(r.Context(), k)
	}
	if err == nil {
		s.Log.InfoContext(r.Context(), "encryption key saved", "name", k.Name, "key", k.Recipient, "allHosts", k.AllHosts, "hosts", k.Hosts)
	}
	respond(w, http.StatusOK, k, err)
}

// deleteEncryptionKey removes a key from the list; files stay encrypted for it until a rekey (Q193).
func (s *server) deleteEncryptionKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.Store.DeleteEncryptionKey(r.Context(), name)
	if err == nil {
		s.Log.InfoContext(r.Context(), "encryption key removed", "name", name)
	}
	respond(w, http.StatusNoContent, nil, err)
}

func saveStatus(r *http.Request) int {
	if r.Method == http.MethodPost {
		return http.StatusCreated
	}
	return http.StatusOK
}

func find[T any](items []T, err error, id int64, idOf func(T) int64) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	for _, it := range items {
		if idOf(it) == id {
			return it, nil
		}
	}
	return zero, domain.ErrNotFound // coverage: unreachable, only a delete racing the save that just returned id
}

func (s *server) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.Store.ListTokens(r.Context())
	respond(w, http.StatusOK, ts, err)
}

func (s *server) createToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	err := decode(w, r, &req)
	if err == nil && strings.TrimSpace(req.Name) == "" {
		err = fmt.Errorf("%w: name is required", domain.ErrInvalid)
	}
	var tok domain.APIToken
	var plain string
	if err == nil {
		tok, plain, err = s.Store.CreateToken(r.Context(), req.Name)
	}
	respond(w, http.StatusCreated, map[string]any{"token": tok, "plaintext": plain}, err)
}

func (s *server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.DeleteToken(r.Context(), id)
	}
	respond(w, http.StatusNoContent, nil, err)
}

// backup sends the backup tar, made in a temporary file first so that a failure is an error, not half a download.
func (s *server) backup(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeStatus(w, http.StatusNotImplemented, "backups are not configured")
		return
	}
	f, err := os.CreateTemp("", "omegacd-backup-*.tar") // owner-only: it holds sealed credentials
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := s.Backup(r.Context(), f); err != nil {
		s.Log.ErrorContext(r.Context(), "backup", "err", err)
		writeError(w, err)
		return
	}
	via := "session"
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		via = "token"
	}
	s.Log.InfoContext(r.Context(), "backup taken", "via", via, "remote", s.clientIP(r))
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="omegacd-backup.tar"`)
	f.Seek(0, io.SeekStart)
	if _, err := io.Copy(w, f); err != nil {
		s.Log.DebugContext(r.Context(), "backup not fully sent: the client left", "err", err)
	}
}
