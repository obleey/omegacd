package engine

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"

	"github.com/obleey/omegacd/internal/domain"
)

const hostRelist = 30 * time.Second

// Watch keeps one Docker event stream per Host (local and every stored Host, re-listed every 30s) and calls
// changed with the Host and Compose project name whenever a container of a Compose project starts, stops,
// dies, is removed, pauses or changes health. After a Host's stream reconnects it calls changed(host, "")
// once: events during the break are lost, so everything on that Host may have changed. It blocks until ctx ends.
func (e *Engine) Watch(ctx context.Context, changed func(host, project string)) error {
	streams := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range streams {
			cancel()
		}
	}()
	for {
		want := map[string]bool{domain.LocalHost: !e.cfg.NoLocalHost}
		if e.cfg.ListHosts != nil {
			hosts, err := e.cfg.ListHosts(ctx)
			if err != nil {
				for name := range streams { // keep what runs
					want[name] = true
				}
				if ctx.Err() == nil {
					slog.Warn("watch: list hosts", "err", err)
				}
			}
			for _, h := range hosts {
				want[h.Name] = true
			}
		}
		for name, cancel := range streams {
			if !want[name] {
				cancel()
				delete(streams, name)
			}
		}
		for name, ok := range want {
			if ok && streams[name] == nil {
				hctx, cancel := context.WithCancel(ctx)
				streams[name] = cancel
				go e.watchHost(hctx, name, changed)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(hostRelist):
		}
	}
}

// watchHost streams one Host's events, reconnecting with backoff. It logs only when the Host goes down or comes back.
func (e *Engine) watchHost(ctx context.Context, host string, changed func(host, project string)) {
	filters := make(client.Filters).Add("type", "container").Add("label", api.ProjectLabel)
	backoff, down := time.Second, false
	for ctx.Err() == nil {
		err := e.streamEvents(ctx, host, filters, changed, func() {
			backoff = time.Second
			if down {
				slog.Info("watch: host reachable again", "host", host)
				down = false
				changed(host, "") // resync: containers may have started or stopped while we weren't listening
			}
		})
		if ctx.Err() != nil {
			return
		}
		if !down {
			slog.Warn("watch: host unreachable, retrying", "host", host, "err", err)
			down = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// streamEvents runs one event stream until it fails; connected is called once the Host answered.
func (e *Engine) streamEvents(ctx context.Context, host string, filters client.Filters, changed func(host, project string), connected func()) error {
	hc, err := e.conn(ctx, host)
	if err != nil {
		return err
	}
	c := hc.cli.Client()
	if _, err := c.Ping(ctx, client.PingOptions{}); err != nil {
		return err
	}
	connected()
	res := c.Events(ctx, client.EventsListOptions{Filters: filters})
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-res.Err:
			return err
		case m := <-res.Messages:
			// healthchecks fire exec_* events every interval; they say nothing about state
			if strings.HasPrefix(string(m.Action), "exec_") {
				continue
			}
			if p := m.Actor.Attributes[api.ProjectLabel]; p != "" {
				changed(host, p)
			}
		}
	}
}
