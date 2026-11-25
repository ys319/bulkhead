package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/forward"
)

// The states of saved forwards as the full-screen interface carries them.
const (
	forwardConnecting = "connecting"
	forwardOpen       = "open"
	forwardElsewhere  = "elsewhere" // another bh has them open
	forwardFailed     = "failed"
)

// forwarder keeps saved forwards open for as long as the full-screen
// interface is: the ones that are on, of the environments that can be
// reached. Each set is one ssh connection of its own, held under the
// set's lock, so that of several bh on a machine one carries it — and
// another takes over within moments when that one ends.
type forwarder struct {
	a     *app
	store config.Store
	ctx   context.Context
	stop  context.CancelFunc
	// changed has a value when there is something new to show.
	changed chan struct{}

	// The pauses between tries; a field so that tests do not wait.
	pause func(ctx context.Context, d time.Duration)

	mu    sync.Mutex
	links map[string]*forwardLink
	done  sync.WaitGroup
}

// forwardLink is one set being carried.
type forwardLink struct {
	signature string
	cancel    context.CancelFunc
	// gone closes when its connection has ended and its lock is free.
	gone   chan struct{}
	state  string
	detail string
}

// forwardWant is a set that should be open now.
type forwardWant struct {
	env config.Environment
	set forward.Set
}

func (w forwardWant) key() string { return w.env.ID + "/" + w.set.Name }

// signature is everything the connection is made from: when it changes,
// the connection is made anew.
func (w forwardWant) signature() string {
	return fmt.Sprintf("%s %s:%d %s", w.env.Type, w.env.Host, w.env.SSHPort, w.set.Summary())
}

func newForwarder(ctx context.Context, a *app, store config.Store) *forwarder {
	ctx, stop := context.WithCancel(ctx)
	return &forwarder{
		a: a, store: store, ctx: ctx, stop: stop,
		changed: make(chan struct{}, 1), links: map[string]*forwardLink{},
		pause: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
	}
}

func (f *forwarder) notify() {
	select {
	case f.changed <- struct{}{}:
	default: // one is waiting already
	}
}

// sync makes what is carried match wants: new ones start, ones that are
// no longer wanted (or are made from something else now) end.
func (f *forwarder) sync(wants []forwardWant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx.Err() != nil {
		return
	}
	wanted := map[string]bool{}
	for _, want := range wants {
		key := want.key()
		wanted[key] = true
		// The one it replaces lets go of the lock first: otherwise this bh
		// would find the set held, by itself.
		var before <-chan struct{}
		if link, running := f.links[key]; running {
			if link.signature == want.signature() {
				continue
			}
			link.cancel()
			before = link.gone
		}
		ctx, cancel := context.WithCancel(f.ctx)
		link := &forwardLink{signature: want.signature(), cancel: cancel, gone: make(chan struct{}), state: forwardConnecting}
		f.links[key] = link
		f.done.Add(1)
		go f.carry(ctx, want, link, before)
	}
	for key, link := range f.links {
		if !wanted[key] {
			link.cancel()
			delete(f.links, key)
		}
	}
}

// state is what is known about a set: "" when it is not being carried.
func (f *forwarder) state(envID, name string) (state, detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if link, running := f.links[envID+"/"+name]; running {
		return link.state, link.detail
	}
	return "", ""
}

func (f *forwarder) set(link *forwardLink, state, detail string) {
	f.mu.Lock()
	changed := link.state != state || link.detail != detail
	link.state, link.detail = state, detail
	f.mu.Unlock()
	if changed {
		f.notify()
	}
}

// close ends every connection and waits until they are gone.
func (f *forwarder) close() {
	f.stop()
	f.done.Wait()
}

// How long a connection has to last before it counts as open (the ssh
// client says nothing when it worked, and fails within moments when a
// port cannot be had), and the pauses between tries.
const (
	forwardSettle    = 1500 * time.Millisecond
	forwardRetryHeld = 3 * time.Second
	forwardRetryMin  = 2 * time.Second
	forwardRetryMax  = 30 * time.Second
)

// carry keeps one set open until ctx ends: it waits while another bh has
// it, and tries again when the connection is lost. before is the end of
// the connection it replaces, if it replaces one.
func (f *forwarder) carry(ctx context.Context, want forwardWant, link *forwardLink, before <-chan struct{}) {
	defer f.done.Done()
	defer close(link.gone)
	if before != nil {
		<-before
	}
	path := f.store.ForwardLock(want.env.ID, want.set.Name)
	args := forwardArgs(f.store, want.env, want.set.Ports)
	retry := forwardRetryMin
	for ctx.Err() == nil {
		lock, err := forward.Acquire(path)
		if errors.Is(err, forward.ErrHeld) {
			holder, _ := forward.Holder(path)
			f.set(link, forwardElsewhere, forward.HeldBy(holder))
			f.pause(ctx, forwardRetryHeld)
			continue
		}
		if err != nil {
			f.set(link, forwardFailed, err.Error())
			f.pause(ctx, forwardRetryMax)
			continue
		}

		f.set(link, forwardConnecting, "")
		started := time.Now()
		settled, cancelSettle := context.WithCancel(ctx)
		go func() {
			f.pause(settled, forwardSettle)
			if settled.Err() == nil {
				f.set(link, forwardOpen, "")
			}
		}()
		var said tail
		code, err := f.a.tunnel(ctx, args, &said)
		cancelSettle()
		lock.Release()
		if ctx.Err() != nil {
			return
		}

		reason := said.last()
		switch {
		case err != nil:
			reason = err.Error()
		case reason == "":
			reason = fmt.Sprintf("ssh ended with status %d", code)
		}
		f.set(link, forwardFailed, reason)
		// A connection that held for a while and then broke is tried again
		// soon; one that fails at once, less and less often.
		if time.Since(started) > forwardRetryMax {
			retry = forwardRetryMin
		}
		f.pause(ctx, retry)
		retry = min(retry*2, forwardRetryMax)
	}
}

// tail keeps the end of what the ssh client said.
type tail struct {
	mu   sync.Mutex
	text []byte
}

func (t *tail) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.text = append(t.text, data...)
	if len(t.text) > 4096 {
		t.text = t.text[len(t.text)-4096:]
	}
	return len(data), nil
}

// last is the last line that says something.
func (t *tail) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(t.text), []byte("\n"))
	return strings.TrimSpace(string(lines[len(lines)-1]))
}

// forwardsSeen is the saved forwards of all environments in a line, to
// notice when another bh (or a hand) changed them.
func forwardsSeen(cfg *config.Config) string {
	var parts []string
	for _, env := range cfg.Envs {
		for _, set := range env.Forwards {
			parts = append(parts, fmt.Sprintf("%s/%s/%t/%s", env.ID, set.Name, set.On, forwardWant{env, set}.signature()))
		}
	}
	return strings.Join(parts, "\n")
}

// watchSettings reports when the saved forwards on disk are no longer the
// ones last seen: every bh edits the same files, and the one that carries
// a set has to hear of a change made in another.
func (f *forwarder) watchSettings(seen string) {
	defer f.done.Done()
	for f.ctx.Err() == nil {
		f.pause(f.ctx, forwardRetryHeld)
		cfg, err := f.store.Load()
		if err != nil {
			continue
		}
		if now := forwardsSeen(cfg); now != seen {
			seen = now
			f.notify()
		}
	}
}
