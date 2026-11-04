package engine

import (
	"context"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/client"
)

// Watch calls changed with the Compose project name whenever a container of any Compose project
// starts, stops, dies, is removed, pauses or changes health. It reconnects until ctx ends.
func (e *Engine) Watch(ctx context.Context, changed func(project string)) error {
	filters := make(client.Filters).Add("type", "container").Add("label", api.ProjectLabel)
	for ctx.Err() == nil {
		res := e.cli.Client().Events(ctx, client.EventsListOptions{Filters: filters})
	stream:
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-res.Err:
				break stream
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
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}
