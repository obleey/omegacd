package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/obleey/dockercd/internal/domain"
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
	err := s.Controller.DeleteApp(r.Context(), r.PathValue("name"), r.URL.Query().Get("cascade") == "true")
	respond(w, http.StatusNoContent, nil, err)
}

func (s *server) syncApp(w http.ResponseWriter, r *http.Request) {
	id, err := s.Controller.Sync(r.Context(), r.PathValue("name"))
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

func (s *server) export(w http.ResponseWriter, r *http.Request) {
	v, err := s.Controller.GetApp(r.Context(), r.PathValue("name"))
	if err != nil {
		writeError(w, err)
		return
	}
	a := v.Application
	out, err := yaml.Marshal(struct {
		Name           string   `yaml:"name"`
		RepoURL        string   `yaml:"repoURL"`
		Path           string   `yaml:"path,omitempty"`
		TargetRevision string   `yaml:"targetRevision,omitempty"`
		ComposeFiles   []string `yaml:"composeFiles,omitempty"`
		Profiles       []string `yaml:"profiles,omitempty"`
		Host           string   `yaml:"host,omitempty"`
		AutoSync       bool     `yaml:"autoSync"`
		SelfHeal       bool     `yaml:"selfHeal"`
		Prune          bool     `yaml:"prune"`
	}{a.Name, a.RepoURL, a.Path, a.TargetRevision, a.ComposeFiles, a.Profiles, a.Host, a.AutoSync, a.SelfHeal, a.Prune})
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

func (s *server) listRepoCreds(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.ListRepoCredentials(r.Context())
	respond(w, http.StatusOK, cs, err)
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
	if err == nil {
		id, err = s.Store.SaveRepoCredential(r.Context(), domain.RepoCredential{
			ID: id, URLPrefix: req.URLPrefix, Username: req.Username, Password: req.Password, SSHPrivateKey: req.SSHPrivateKey,
		})
	}
	var saved domain.RepoCredential
	if err == nil {
		var cs []domain.RepoCredential
		cs, err = s.Store.ListRepoCredentials(r.Context())
		saved, err = find(cs, err, id, func(c domain.RepoCredential) int64 { return c.ID })
	}
	respond(w, saveStatus(r), saved, err)
}

func (s *server) deleteRepoCred(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err == nil {
		err = s.Store.DeleteRepoCredential(r.Context(), id)
	}
	respond(w, http.StatusNoContent, nil, err)
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
