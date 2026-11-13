package engine

import (
	"context"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
)

// Watch calls changed with the Compose project name whenever a container of a Compose project starts, stops, dies, is
// removed, pauses or changes health. After the event stream reconnects it calls changed("") once: events during the
// break are lost, so anything may have changed. It blocks, reconnecting with backoff, until ctx ends.
func (e *Engine) Watch(ctx context.Context, changed func(project string)) error {
	// only by type: Podman 4.9 (Ubuntu 24.04's) matches no event to a label filter without a value, so the project
	// label is checked on each event below
	filters := make(client.Filters).Add("type", "container")
	backoff, down := time.Second, false
	for ctx.Err() == nil {
		err := e.streamEvents(ctx, filters, changed, func() {
			backoff = time.Second
			if down {
				e.cfg.Log.Info("watch: engine reachable again")
				down = false
				changed("") // resync: containers may have started or stopped while we weren't listening
			}
		})
		if ctx.Err() != nil {
			return nil
		}
		if !down {
			e.cfg.Log.Warn("watch: engine unreachable, retrying", "err", err)
			down = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
	return nil
}

// streamEvents runs one event stream until it fails; connected is called once the engine answered.
func (e *Engine) streamEvents(ctx context.Context, filters client.Filters, changed func(project string), connected func()) error {
	c := e.hc.cli.Client()
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
				changed(p)
			}
		}
	}
}
