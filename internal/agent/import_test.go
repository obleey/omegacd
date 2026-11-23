package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/obleey/omegacd/internal/importer"
	"github.com/obleey/omegacd/internal/logging"
	agentv1 "github.com/obleey/omegacd/internal/proto/agent/v1"
)

// stacksHost runs one stack, with a compose file and nothing else, as importer.Host.
type stacksHost struct{ closed *bool }

func (stacksHost) Stacks(context.Context) ([]importer.Stack, error) {
	return []importer.Stack{{Name: "blog", Dir: "/srv/blog", Files: []string{"compose.yaml"}}, {Name: "ours", Managed: true}}, nil
}
func (stacksHost) Info(context.Context, string, []string) (map[string]importer.Info, error) {
	return map[string]importer.Info{}, nil
}
func (stacksHost) Read(_ context.Context, _ string, paths []string) (map[string][]byte, []string, error) {
	if len(paths) > 0 && paths[0] == "compose.yaml" {
		return map[string][]byte{"compose.yaml": []byte("services:\n  web:\n    image: nginx\n")}, nil, nil
	}
	return map[string][]byte{}, nil, nil
}
func (stacksHost) Storage(context.Context) (map[string]string, error) { return nil, nil }
func (h stacksHost) Close() error                                     { *h.closed = true; return nil }

// M6-05: the Agent answers an ImportRequest with every stack's result, its files beside the JSON (Q319), then done;
// what stops it is in the done answer and logged at Warn (Q132).
func TestAnswerImport(t *testing.T) {
	logs := &logging.Buffer{}
	ag := New(nil, nil, nil, nil, nil, slog.New(slog.NewTextHandler(logs, nil)))
	closed := false
	ag.importHost = func() (importHost, error) { return stacksHost{&closed}, nil }
	a, cp := pair(t)
	req, _ := json.Marshal(importer.Request{AppsDir: "apps", HostName: "vps"})
	ag.answerImport(t.Context(), a, 7, &agentv1.ImportRequest{Request: req})
	var results []importer.Result
	for {
		e := recv(t, cp)
		r := e.GetImportResult()
		if e.ReplyTo != 7 || r == nil {
			t.Fatalf("got %v", e)
		}
		if r.Done {
			if r.Error != "" {
				t.Fatal(r.Error)
			}
			break
		}
		var res importer.Result
		if err := json.Unmarshal(r.Result, &res); err != nil || res.Files != nil {
			t.Fatalf("the JSON holds its files? %v %s", err, r.Result)
		}
		res.Files = r.Files
		results = append(results, res)
	}
	if len(results) != 2 || results[0].Name != "blog" || !strings.Contains(string(results[0].Files["compose.yaml"]), "nginx") ||
		!strings.Contains(string(results[0].Definition), "host: vps") || results[1].Skip != "managed" || !closed {
		t.Fatalf("results %+v, closed %v", results, closed)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="import answered"`) {
		t.Fatalf("logs:\n%s", logs.String())
	}

	for name, tc := range map[string]struct {
		req  []byte
		host func() (importHost, error)
		want string
	}{
		"a request it can't read": {[]byte("{"), nil, "unexpected end of JSON input"},
		"no engine":               {req, func() (importHost, error) { return nil, errors.New("no socket") }, "no socket"},
		"a stack not there":       {[]byte(`{"Only":["nope"]}`), nil, `no Compose stack`},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.host != nil {
				ag.importHost = tc.host
			} else {
				ag.importHost = func() (importHost, error) { return stacksHost{&closed}, nil }
			}
			ag.answerImport(t.Context(), a, 8, &agentv1.ImportRequest{Request: tc.req})
			if r := recv(t, cp).GetImportResult(); !r.GetDone() || !strings.Contains(r.GetError(), tc.want) {
				t.Fatalf("got %v", r)
			}
			if !strings.Contains(logs.String(), "level=WARN msg=import err=") || !strings.Contains(logs.String(), tc.want) {
				t.Fatalf("logs:\n%s", logs.String())
			}
		})
	}
}
