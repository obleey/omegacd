package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obleey/omegacd/internal/agents"
	"github.com/obleey/omegacd/internal/appdef"
	"github.com/obleey/omegacd/internal/domain"
	"github.com/obleey/omegacd/internal/source"
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

func (s *server) deleteApp(w http.ResponseWriter, r *http.Request) {
	err := s.Controller.DeleteApp(r.Context(), r.PathValue("name"), r.URL.Query().Get("cascade") == "true", r.URL.Query().Get("volumes") == "true")
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
	if err != nil {
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
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
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
	if provider != "github" && provider != "gitlab" && provider != "gitea" {
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

func (s *server) listHosts(w http.ResponseWriter, r *http.Request) {
	hs, err := s.Controller.ListHosts(r.Context())
	respond(w, http.StatusOK, hs, err)
}

func (s *server) saveHost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		WorkDir       string `json:"workDir"`
		SSHPrivateKey string `json:"sshPrivateKey"`
		TLSCA         string `json:"tlsCA"`
		TLSCert       string `json:"tlsCert"`
		TLSKey        string `json:"tlsKey"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	if name := r.PathValue("name"); name != "" {
		if req.Name != "" && req.Name != name {
			writeError(w, fmt.Errorf("%w: name cannot be changed", domain.ErrInvalid))
			return
		}
		req.Name = name
	}
	v, err := s.Controller.SaveHost(r.Context(), domain.Host{
		Name: req.Name, URL: req.URL, WorkDir: req.WorkDir,
		SSHPrivateKey: req.SSHPrivateKey, TLSCA: req.TLSCA, TLSCert: req.TLSCert, TLSKey: req.TLSKey,
	}, r.Method == http.MethodPost)
	respond(w, saveStatus(r), v, err)
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

func (s *server) testHost(w http.ResponseWriter, r *http.Request) {
	st, err := s.Controller.TestHost(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, st, err)
}

// Host Keys: responses carry public keys only (HostKey.PrivateKey is never serialised).
func (s *server) hostKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Controller.HostKeys(r.Context(), r.PathValue("name"))
	respond(w, http.StatusOK, keys, err)
}

func (s *server) generateHostKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.Controller.GenerateHostKey(r.Context(), r.PathValue("name"))
	respond(w, http.StatusCreated, k, err)
}

func (s *server) importHostKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PrivateKey string `json:"privateKey"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, err)
		return
	}
	k, err := s.Controller.ImportHostKey(r.Context(), r.PathValue("name"), req.PrivateKey)
	respond(w, http.StatusCreated, k, err)
}

func (s *server) removeHostKey(w http.ResponseWriter, r *http.Request) {
	respond(w, http.StatusNoContent, nil, s.Controller.RemoveHostKey(r.Context(), r.PathValue("name"), r.PathValue("recipient")))
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
	return zero, domain.ErrNotFound
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
