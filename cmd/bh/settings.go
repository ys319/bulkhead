package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/ys319/bulkhead/internal/config"
)

const settingsUsage = `usage: bh settings [--shm-size <size>]

  What holds for all environments of this machine. Without a flag, prints
  the settings.

  --shm-size <size>   how much /dev/shm holds in an environment: a number
                      with m or g (512m, 16g; 8g unless set). It is a
                      ceiling: memory is used only for what is written
                      there. An environment has the new size once its dev
                      container is created again (bh restart <env>)`

// machineSettings are this machine's settings as they are in force.
func (a *app) machineSettings() (config.Settings, error) {
	store, err := a.store()
	if err != nil {
		return config.Settings{}, err
	}
	return store.LoadSettings()
}

// setShmSize changes how much /dev/shm holds in every environment. The
// containers that exist keep what they were created with: the next
// bh restart (or the bh up that finds them changed) recreates them.
func (a *app) setShmSize(text string) (config.Settings, error) {
	store, err := a.store()
	if err != nil {
		return config.Settings{}, err
	}
	size, err := config.ParseShmSize(text)
	if err != nil {
		return config.Settings{}, usagef("--shm-size: %v", err)
	}
	settings, err := store.LoadSettings()
	if err != nil {
		return settings, err
	}
	settings.ShmSize = size
	return settings, store.SaveSettings(settings)
}

func (a *app) settings(_ context.Context, args []string) error {
	flags := flag.NewFlagSet("settings", flag.ContinueOnError)
	shmSize := flags.String("shm-size", "", "")
	rest, err := parseFlags(flags, args)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return usagef("%s", settingsUsage)
	}
	changed := false
	flags.Visit(func(f *flag.Flag) { changed = changed || f.Name == "shm-size" })
	if !changed {
		settings, err := a.machineSettings()
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "shm-size  %s\n", config.FormatSize(settings.ShmSize))
		return nil
	}
	settings, err := a.setShmSize(*shmSize)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "/dev/shm holds up to %s in an environment once its dev container is created again (bh restart <env>)\n",
		config.FormatSize(settings.ShmSize))
	return nil
}
