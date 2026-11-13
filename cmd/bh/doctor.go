package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ys319/bulkhead/internal/config"
	"github.com/ys319/bulkhead/internal/doctor"
	"github.com/ys319/bulkhead/internal/engine"
	"github.com/ys319/bulkhead/internal/plan"
	"github.com/ys319/bulkhead/internal/sshconfig"
)

// doctor checks this machine's setup and the containers of the named
// environment, or of all of them. Any failed check makes the exit status 1;
// warnings do not.
func (a *app) doctor(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return usagef("usage: bh doctor [<env>]")
	}
	ref := ""
	if len(rest) == 1 {
		ref = rest[0]
	}
	sections, err := a.diagnose(ctx, ref)
	if err != nil {
		return err
	}
	printDiagnosis(a.stdout, sections)
	if doctor.Failures(sections) > 0 {
		return exitCode(1)
	}
	return nil
}

// diagnose gathers the facts and runs the checks of internal/doctor: the
// machine first, then the environment called ref, or every environment
// when ref is empty. It only reads — the configuration directory, the
// user's ssh configuration and the engine's inspections; unlike bh up it
// makes no key, no init.sh, no image and no probe container.
func (a *app) diagnose(ctx context.Context, ref string) ([]doctor.Section, error) {
	store, cfg, err := a.load()
	if err != nil {
		return nil, err
	}
	envs := cfg.Envs
	if ref != "" {
		env, err := cfg.Find(ref)
		if err != nil {
			return nil, err
		}
		envs = []config.Environment{env}
	}

	// One question decides whether the containers can be looked at, so
	// that an engine that is down costs one timeout and not one per
	// environment.
	var info *engine.Info
	eng, disconnect, engineErr := a.dial(a.getenv)
	if engineErr == nil {
		defer disconnect()
		asking, cancel := context.WithTimeout(ctx, 10*time.Second)
		answer, err := eng.Info(asking)
		cancel()
		if err != nil {
			engineErr = err
		} else {
			info = &answer
		}
	}
	if info == nil {
		eng = nil
	}
	registry, registryErr := store.LoadKeys()

	sections := []doctor.Section{{
		Title:    "machine",
		Findings: doctor.CheckMachine(a.machineFacts(store, cfg, info, engineErr, len(registry), registryErr)),
	}}
	for _, env := range envs {
		if !env.Local() {
			sections = append(sections, doctor.Section{Title: env.Name, Findings: doctor.CheckRemote()})
			continue
		}
		sections = append(sections, doctor.Section{
			Title:    env.Name,
			Findings: checkEnvironment(ctx, eng, store, env, registry, registryErr),
		})
	}
	// With a name, Find has already stopped at a broken environment.
	if ref == "" {
		for _, broken := range cfg.Broken {
			sections = append(sections, doctor.Section{Title: broken.ID, Findings: doctor.CheckBroken(broken.Problem)})
		}
	}
	return sections, nil
}

// checkEnvironment is the findings about one local environment: its
// settings held against its containers as the engine has them now. A nil
// engine is one that does not answer; the containers are then not looked
// at. registry and registryErr are the outcome of loading the key registry.
func checkEnvironment(ctx context.Context, eng dockerEngine, store config.Store, env config.Environment,
	registry []config.KeyEntry, registryErr error) []doctor.Finding {
	facts := environmentFacts(store, env, registry, registryErr)
	if eng == nil {
		facts.EngineDown = true
	} else {
		facts.Holder, facts.InspectErr = eng.Describe(ctx, plan.HolderName(env.ID))
		if facts.InspectErr == nil {
			facts.Dev, facts.InspectErr = eng.Describe(ctx, plan.DevName(env.ID))
		}
	}
	return doctor.CheckLocal(facts)
}

func fileFact(path string) doctor.File {
	info, err := os.Stat(path)
	if err != nil {
		return doctor.File{Path: path, Err: err}
	}
	return doctor.File{Path: path, Mode: info.Mode()}
}

func (a *app) machineFacts(store config.Store, cfg *config.Config, info *engine.Info, engineErr error, keys int, keysErr error) doctor.Machine {
	inPath := func(name string) bool {
		_, err := a.lookPath(name)
		return err == nil
	}
	facts := doctor.Machine{
		GOOS:   a.goos,
		Engine: info, EngineErr: engineErr,
		DockerCLI: inPath("docker"), SSH: inPath("ssh"), VSCode: a.hasVSCode(),
		MachineKey: fileFact(store.MachineKey()), InitScript: fileFact(store.InitScript()),
		Keys: keys, KeysErr: keysErr,
		Environments: len(cfg.Envs) + len(cfg.Broken),
	}

	path, err := a.sshConfigPath()
	if err != nil {
		facts.SSHConfigErr = err
		return facts
	}
	facts.SSHConfig = path
	if facts.Included, err = sshconfig.Included(path); err != nil {
		facts.SSHConfigErr = err
		return facts
	}
	if facts.IncludeBroken, err = sshconfig.Broken(path); err != nil {
		facts.SSHConfigErr = err
		return facts
	}
	entries, err := os.ReadFile(sshconfig.FragmentPath(path))
	switch {
	case err == nil:
		facts.EntriesCurrent = string(entries) == sshconfig.Fragment(store.MachineKey(), sshHosts(cfg))
	case !errors.Is(err, fs.ErrNotExist):
		facts.SSHConfigErr = err
	}
	return facts
}

// environmentFacts is what bh would create the environment's containers
// from now: containersFor and realize without what they make. The groups
// realize gets from probe containers stay zero; the checks read them off
// the dev container.
func environmentFacts(store config.Store, env config.Environment, registry []config.KeyEntry, registryErr error) doctor.Environment {
	facts := doctor.Environment{Name: env.Name}
	unknown := func(why string) {
		if facts.PlanUnknown == "" {
			facts.PlanUnknown = why
		}
	}

	var keys []string
	if machineKey, err := os.ReadFile(store.MachineKey() + ".pub"); err != nil {
		unknown("this machine's public key cannot be read (bh key show makes or restores it)")
	} else {
		keys = append(keys, strings.TrimSpace(string(machineKey)))
	}
	if registryErr != nil {
		unknown("the key registry cannot be read")
	} else if registered, err := config.ResolveKeys(registry, env); err != nil {
		facts.KeysErr = err
		unknown("a key of the environment does not resolve")
	} else {
		keys = append(keys, registered...)
	}
	facts.Plan = planEnv(env, keys, store.InitScript())
	// Docker Desktop reports the bind by the file's real path.
	if followed, err := filepath.EvalSymlinks(store.InitScript()); err == nil && followed != store.InitScript() {
		facts.Resolved = map[string]string{store.InitScript(): followed}
	}

	owner, err := store.Owner()
	if err != nil {
		unknown(err.Error())
	}
	facts.Target = plan.Target{Owner: owner}
	facts.Target.HolderImage, facts.Target.DevImage, err = plannedImages(store, env.ID)
	if err != nil {
		unknown(fmt.Sprintf("the images cannot be told: %v", err))
	}
	return facts
}

// plannedImages are the references realize creates the containers from,
// without building anything: a hash of the sources is all a reference is.
func plannedImages(store config.Store, id string) (holder, dev string, err error) {
	if holder, err = holderSource().Ref(); err != nil {
		return "", "", err
	}
	if dev, err = devBaseSource().Ref(); err != nil {
		return "", "", err
	}
	switch _, statErr := os.Stat(store.DevDockerfile(id)); {
	case statErr == nil:
		if dev, err = devSource(store, id, dev).Ref(); err != nil {
			return "", "", err
		}
	case !errors.Is(statErr, fs.ErrNotExist):
		return "", "", statErr
	}
	return holder, dev, nil
}

// The marks of the TypeScript version: the same width, the failure in
// capitals so that it stands out without colour.
var levelMark = map[doctor.Level]string{
	doctor.OK:   "  ok  ",
	doctor.Warn: " warn ",
	doctor.Fail: " FAIL ",
	doctor.Skip: " skip ",
}

func printDiagnosis(out io.Writer, sections []doctor.Section) {
	for _, section := range sections {
		fmt.Fprintf(out, "== %s ==\n", section.Title)
		for _, finding := range section.Findings {
			// Further lines of a message (an error with details) line up
			// under the first.
			message := strings.ReplaceAll(strings.TrimRight(finding.Message, "\n"), "\n", "\n       ")
			fmt.Fprintf(out, "%s %s\n", levelMark[finding.Level], message)
		}
		fmt.Fprintln(out)
	}
	if failures := doctor.Failures(sections); failures > 0 {
		fmt.Fprintf(out, "doctor: %d failure(s)\n", failures)
	} else {
		fmt.Fprintln(out, "doctor: no failures")
	}
}
