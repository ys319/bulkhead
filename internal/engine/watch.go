package engine

import (
	"context"
	"strings"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"
)

// refreshActions are the container events after which an environment's
// state may read differently. The rest — above all the exec_* events that
// every healthcheck produces, every few seconds — say nothing new.
var refreshActions = map[events.Action]bool{
	events.ActionCreate: true, events.ActionStart: true, events.ActionRestart: true,
	events.ActionStop: true, events.ActionKill: true, events.ActionDie: true,
	events.ActionDestroy: true, events.ActionPause: true, events.ActionUnPause: true,
	events.ActionRename: true, events.ActionUpdate: true, events.ActionOOM: true,
}

// Watch reports changes of containers that carry the label: a value on
// changes for each one, until the context ends or the connection breaks —
// then the error (nil when the context ended) is sent on done.
func (c *Client) Watch(ctx context.Context, label string) (changes <-chan struct{}, done <-chan error) {
	out := make(chan struct{})
	end := make(chan error, 1)
	stream := c.api.Events(ctx, client.EventsListOptions{
		Filters: make(client.Filters).Add("type", string(events.ContainerEventType)).Add("label", label),
	})
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				end <- nil
				return
			case err := <-stream.Err:
				if ctx.Err() != nil {
					err = nil
				}
				end <- err
				return
			case message := <-stream.Messages:
				if !refreshActions[message.Action] && !strings.HasPrefix(string(message.Action), "health_status") {
					continue
				}
				select {
				case out <- struct{}{}:
				case <-ctx.Done():
					end <- nil
					return
				}
			}
		}
	}()
	return out, end
}
