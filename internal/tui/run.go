package tui

import (
	"context"
	"errors"
	"os"
	"os/signal"

	tea "charm.land/bubbletea/v2"
)

// Run shows the interface until the user leaves it.
func Run(ctx context.Context, backend Backend) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	program := tea.NewProgram(New(ctx, backend), tea.WithContext(ctx), tea.WithoutSignalHandler())

	// Interrupts are taken here and dropped. At the keyboard, Ctrl-C reaches
	// the interface as a key. As a signal it arrives while another program
	// has the terminal (following logs ends with Ctrl-C): that program is
	// the one it is meant for, and this one must still be there afterwards.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	go func() {
		for {
			select {
			case <-interrupts:
			case <-ctx.Done():
				return
			}
		}
	}()
	stop := make(chan os.Signal, 1)
	notifyTermination(stop)
	defer signal.Stop(stop)
	go func() {
		select {
		case <-stop:
			program.Quit()
		case <-ctx.Done():
		}
	}()

	_, err := program.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}
