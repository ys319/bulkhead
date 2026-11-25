package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/forward"
)

const forwardUsage = `usage: bh forward <subcommand>

  forward run <env> [<name or port>...]
                      carry ports until Ctrl-C: saved ones by name, others
                      as they are written, in any mix. Without any, all
                      saved ones that are on
  forward save <env> <name> <port>... [--no-run]
                      save ports under a name (an existing name is
                      replaced), switched on, and open them as run does
  forward on <env> <name>
  forward off <env> <name>
                      whether run without a name, and the full-screen
                      interface while it is open, open them
  forward remove <env> <name>
  forward list [<env>]
                      the saved ones, and whether a bh has them open now

  A port is written as Docker writes a published one: this machine's port,
  then the environment's.

    3000              the environment's 3000, at localhost:3000 here
    8080:80           the environment's 80, at localhost:8080 here
    host:8000         this machine's 8000, at localhost:8000 in the environment
    host:80:8080      this machine's 80, at localhost:8080 in the environment

  With host: the environment reaches a service of this machine, for as
  long as the forward is open. Both sides listen on localhost only. The
  ports travel over the environment's ssh connection, so all of this is
  the same for an environment on another machine (bh remote). Saved ones
  of one name are open in one bh at a time.`

// forwardState is a saved set and who has it open now.
type forwardState struct {
	forward.Set
	// Held: a bh has it open; Holder is that process, 0 when it is not known.
	Held   bool
	Holder int
}

// forwardStates are the environment's saved forwards as they are now.
func forwardStates(store config.Store, env config.Environment) []forwardState {
	out := make([]forwardState, len(env.Forwards))
	for i, set := range env.Forwards {
		out[i].Set = set
		out[i].Holder, out[i].Held = forward.Holder(store.ForwardLock(env.ID, set.Name))
	}
	return out
}

// label is the state of a set in a few words.
func (s forwardState) label() string {
	text := "off"
	if s.On {
		text = "on"
	}
	if s.Held {
		text += ", open"
		if s.Holder > 0 {
			text += fmt.Sprintf(" (process %d)", s.Holder)
		}
	}
	return text
}

// changeForwards edits the saved forwards of an environment, local or
// remote: they are part of how this machine uses it.
func (a *app) changeForwards(ref string, edit func(env config.Environment, sets []forward.Set) ([]forward.Set, error)) (config.Environment, error) {
	store, cfg, err := a.load()
	if err != nil {
		return config.Environment{}, err
	}
	env, err := cfg.Find(ref)
	if err != nil {
		return env, err
	}
	sets, err := edit(env, slices.Clone(env.Forwards))
	if err != nil {
		return env, err
	}
	env.Forwards = sets
	return env, store.SaveEnvironment(env)
}

// saveForward saves ports under a name, switched on. An existing set of
// that name is replaced where it stands.
func (a *app) saveForward(ref, name string, words []string) (env config.Environment, saved forward.Set, replaced bool, err error) {
	if err := forward.CheckName(name); err != nil {
		return env, saved, false, usagef("%q: %v", name, err)
	}
	if len(words) == 0 {
		return env, saved, false, usagef("usage: bh forward save <env> <name> <port>...")
	}
	ports, err := forward.ParseAll(words)
	if err != nil {
		return env, saved, false, usagef("%v", err)
	}
	saved = forward.Set{Name: name, On: true, Ports: ports}
	env, err = a.changeForwards(ref, func(_ config.Environment, sets []forward.Set) ([]forward.Set, error) {
		for i := range sets {
			if sets[i].Name == name {
				sets[i], replaced = saved, true
				return sets, nil
			}
		}
		return append(sets, saved), nil
	})
	return env, saved, replaced, err
}

func noSuchForward(env config.Environment, name string) error {
	return fmt.Errorf("%q has no saved forwards named %q (bh forward list %s)", env.Name, name, env.Name)
}

// switchForward turns a saved set on or off.
func (a *app) switchForward(ref, name string, on bool) (config.Environment, error) {
	return a.changeForwards(ref, func(env config.Environment, sets []forward.Set) ([]forward.Set, error) {
		for i := range sets {
			if sets[i].Name == name {
				sets[i].On = on
				return sets, nil
			}
		}
		return nil, noSuchForward(env, name)
	})
}

// removeForward forgets a saved set. A bh that has it open goes on until
// it ends or (the full-screen interface) notices.
func (a *app) removeForward(ref, name string) (config.Environment, error) {
	return a.changeForwards(ref, func(env config.Environment, sets []forward.Set) ([]forward.Set, error) {
		kept := slices.DeleteFunc(sets, func(set forward.Set) bool { return set.Name == name })
		if len(kept) == len(env.Forwards) {
			return nil, noSuchForward(env, name)
		}
		return kept, nil
	})
}

// forwarding is what one bh forward run opens.
type forwarding struct {
	env config.Environment
	// sets are the saved ones among it, to be held while it is open.
	sets []forward.Set
	// lines say what is carried, one port each.
	lines []string
	ports []forward.Port
}

// planForwarding reads the words of a run: names of saved sets and ports,
// in any mix. Without words it is every saved set that is on.
func planForwarding(env config.Environment, words []string) (forwarding, error) {
	plan := forwarding{env: env}
	add := func(port forward.Port, from string) {
		line := port.Describe()
		if from != "" {
			line += "  (" + from + ")"
		}
		plan.ports, plan.lines = append(plan.ports, port), append(plan.lines, line)
	}
	take := func(set forward.Set) {
		plan.sets = append(plan.sets, set)
		for _, port := range set.Ports {
			add(port, set.Name)
		}
	}
	if len(words) == 0 {
		for _, set := range env.Forwards {
			if set.On {
				take(set)
			}
		}
		if len(plan.sets) == 0 {
			return plan, fmt.Errorf("%q has no saved forwards that are on — name ports (bh forward run %s 3000), or save some (bh forward save %s <name> <port>...)", env.Name, env.Name, env.Name)
		}
	}
	for _, word := range words {
		if forward.IsPort(word) {
			port, err := forward.Parse(word)
			if err != nil {
				return plan, usagef("%v", err)
			}
			add(port, "")
			continue
		}
		set, found := forward.Find(env.Forwards, word)
		if !found {
			if forward.CheckName(word) != nil {
				// Neither a port nor something a name could be.
				_, err := forward.Parse(word)
				return plan, usagef("%v", err)
			}
			return plan, noSuchForward(env, word)
		}
		if slices.ContainsFunc(plan.sets, func(taken forward.Set) bool { return taken.Name == set.Name }) {
			return plan, usagef("%q is named twice", word)
		}
		take(set)
	}
	if err := forward.Check(plan.ports); err != nil {
		return plan, err
	}
	return plan, nil
}

// errForwardHeld: a saved set the run asked for is open in another bh.
type errForwardHeld struct {
	env, name string
	holder    int
}

func (e errForwardHeld) Error() string {
	return fmt.Sprintf("%q of %q is open in %s — one bh at a time has saved forwards open", e.name, e.env, forward.HeldBy(e.holder))
}

// openForwarding carries the ports until the ssh client ends: at Ctrl-C,
// or when the connection is lost. The saved sets among them are held for
// that time.
func (a *app) openForwarding(ctx context.Context, store config.Store, plan forwarding) error {
	if err := a.requireRunning(ctx, plan.env); err != nil {
		return err
	}
	if _, err := ensureMachineKey(store); err != nil {
		return err
	}
	for _, set := range plan.sets {
		path := store.ForwardLock(plan.env.ID, set.Name)
		lock, err := forward.Acquire(path)
		if errors.Is(err, forward.ErrHeld) {
			holder, _ := forward.Holder(path)
			return errForwardHeld{plan.env.Name, set.Name, holder}
		}
		if err != nil {
			return err
		}
		defer lock.Release()
	}

	fmt.Fprintf(a.stdout, "Forwarding for %q until Ctrl-C:\n", plan.env.Name)
	for _, line := range plan.lines {
		fmt.Fprintf(a.stdout, "  %s\n", line)
	}
	// A forward ends with the bh that opened it, however that is told to
	// end: Ctrl-C, a kill, the terminal going away.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code, err := a.tunnel(ctx, forwardArgs(store, plan.env, plan.ports), a.stderr)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		// The way a run is meant to end.
		fmt.Fprintf(a.stdout, "\nNo longer forwarding for %q\n", plan.env.Name)
		return nil
	}
	// The ssh client said why on its own (a port in use, no connection).
	return fmt.Errorf("the forwards of %q are closed: ssh ended with status %d", plan.env.Name, code)
}

// forwardArgs are the arguments of the ssh client for a connection that
// carries the ports and nothing else.
func forwardArgs(store config.Store, env config.Environment, ports []forward.Port) []string {
	args := append([]string{"-N"}, forward.SSHArgs(ports)...)
	return append(args, sshArgs(store, env, forward.SessionOptions()...)...)
}

func (a *app) forward(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("%s", forwardUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "run":
		if len(rest) == 0 {
			return usagef("usage: bh forward run <env> [<name or port>...]")
		}
		store, cfg, err := a.load()
		if err != nil {
			return err
		}
		env, err := cfg.Find(rest[0])
		if err != nil {
			return err
		}
		plan, err := planForwarding(env, rest[1:])
		if err != nil {
			return err
		}
		return a.openForwarding(ctx, store, plan)

	case "save":
		flags := flag.NewFlagSet("forward save", flag.ContinueOnError)
		noRun := flags.Bool("no-run", false, "")
		words, err := parseFlags(flags, rest)
		if err != nil {
			return err
		}
		if len(words) < 3 {
			return usagef("usage: bh forward save <env> <name> <port>... [--no-run]")
		}
		env, saved, replaced, err := a.saveForward(words[0], words[1], words[2:])
		if err != nil {
			return err
		}
		verb := "Saved"
		if replaced {
			verb = "Replaced"
		}
		fmt.Fprintf(a.stdout, "%s %q for %q: %s\n", verb, saved.Name, env.Name, saved.Summary())
		fmt.Fprintf(a.stdout, "From now on: bh forward run %s %s\n", env.Name, saved.Name)
		if *noRun {
			return nil
		}
		// Saved is saved: what stands in the way of opening it now is said,
		// and is no failure of the command.
		store, err := a.store()
		if err != nil {
			return err
		}
		plan, err := planForwarding(env, []string{saved.Name})
		if err != nil {
			return err
		}
		fmt.Fprintln(a.stdout)
		err = a.openForwarding(ctx, store, plan)
		var held errForwardHeld
		switch {
		case errors.As(err, &held):
			fmt.Fprintf(a.stdout, "Not opened here: %v\n", err)
			return nil
		case err != nil && plan.env.Local() && a.requireRunning(ctx, plan.env) != nil:
			fmt.Fprintf(a.stdout, "Not opened now: %v\n", err)
			return nil
		}
		return err

	case "on", "off":
		if len(rest) != 2 {
			return usagef("usage: bh forward %s <env> <name>", sub)
		}
		env, err := a.switchForward(rest[0], rest[1], sub == "on")
		if err != nil {
			return err
		}
		if sub == "on" {
			fmt.Fprintf(a.stdout, "%q of %q is on: bh forward run %s and the full-screen interface open it\n", rest[1], env.Name, env.Name)
		} else {
			fmt.Fprintf(a.stdout, "%q of %q is off: it is opened only by name (bh forward run %s %s)\n", rest[1], env.Name, env.Name, rest[1])
		}
		return nil

	case "remove":
		if len(rest) != 2 {
			return usagef("usage: bh forward remove <env> <name>")
		}
		env, err := a.removeForward(rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed %q from %q\n", rest[1], env.Name)
		return nil

	case "list":
		if len(rest) > 1 {
			return usagef("usage: bh forward list [<env>]")
		}
		store, cfg, err := a.load()
		if err != nil {
			return err
		}
		envs := cfg.Envs
		if len(rest) == 1 {
			env, err := cfg.Find(rest[0])
			if err != nil {
				return err
			}
			envs = []config.Environment{env}
		}
		table := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		rows := 0
		for _, env := range envs {
			for _, state := range forwardStates(store, env) {
				if rows == 0 {
					fmt.Fprintln(table, "ENV\tNAME\tSTATE\tPORTS")
				}
				rows++
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", env.Name, state.Name, state.label(), state.Summary())
			}
		}
		if rows == 0 {
			fmt.Fprintln(a.stdout, "No saved forwards (bh forward save <env> <name> <port>...)")
			return nil
		}
		return table.Flush()
	}
	return usagef("%s", forwardUsage)
}

// forwardNote is what a list of environments says about one's forwards:
// the saved ones a bh has open now.
func forwardNote(store config.Store, env config.Environment) string {
	var open []string
	for _, state := range forwardStates(store, env) {
		if state.Held {
			open = append(open, state.Name)
		}
	}
	if len(open) == 0 {
		return ""
	}
	return "forwarding " + strings.Join(open, ", ")
}
