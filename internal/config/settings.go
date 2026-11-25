package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Settings are what holds for every environment of this machine
// (settings.json). They are not part of an environment: a copy, a
// snapshot or an export of one does not carry them, and another machine
// has its own.
type Settings struct {
	// ShmSize is the size of /dev/shm in an environment, in bytes.
	//
	// It is a ceiling, not a reservation: /dev/shm is memory, used only
	// for what is written there, and nothing limits an environment's
	// memory otherwise. The engine's own default of 64 MB is too small
	// for a browser under test and for anything that passes arrays between
	// processes (a data loader's workers), and what running out looks like
	// — a crashed tab, a bus error — does not say what ran out.
	ShmSize int64
}

const (
	mebibyte = 1 << 20
	gibibyte = 1 << 30

	// DefaultShmSize covers what the programs that use /dev/shm are
	// usually told to ask for. A machine with less memory than that is
	// better off with a smaller one (bh settings --shm-size).
	DefaultShmSize = 8 * gibibyte
	// MinShmSize is what the engine gives a container that asks for
	// nothing: less than that is no setting anybody means.
	MinShmSize = 64 * mebibyte
)

// DefaultSettings are the settings of a machine that has set none.
func DefaultSettings() Settings { return Settings{ShmSize: DefaultShmSize} }

// SettingsFile is the file of this machine's settings. It is there once
// one was changed.
func (s Store) SettingsFile() string { return filepath.Join(s.Dir, "settings.json") }

var sizePattern = regexp.MustCompile(`^([0-9]{1,6})([mg])b?$`)

// ParseSize reads a size as it is written in settings.json and on the
// command line: a whole number of mebibytes (512m) or gibibytes (8g).
func ParseSize(text string) (int64, error) {
	match := sizePattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(text)))
	if match == nil {
		return 0, fmt.Errorf("%q is not a size: a number with m or g (512m, 8g)", text)
	}
	count, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	if match[2] == "g" {
		return count * gibibyte, nil
	}
	return count * mebibyte, nil
}

// FormatSize writes a size the way ParseSize reads it.
func FormatSize(bytes int64) string {
	if bytes%gibibyte == 0 {
		return strconv.FormatInt(bytes/gibibyte, 10) + "g"
	}
	return strconv.FormatInt(bytes/mebibyte, 10) + "m"
}

// ParseShmSize reads a size for /dev/shm and refuses one that cannot be it.
func ParseShmSize(text string) (int64, error) {
	size, err := ParseSize(text)
	if err != nil {
		return 0, err
	}
	return size, checkShmSize(size)
}

func checkShmSize(size int64) error {
	switch {
	case size < MinShmSize:
		return fmt.Errorf("/dev/shm takes at least %s (what Docker gives a container that asks for nothing)", FormatSize(MinShmSize))
	case size%mebibyte != 0:
		return errors.New("/dev/shm takes a whole number of m or g")
	}
	return nil
}

func (s Settings) validate() error {
	if err := checkShmSize(s.ShmSize); err != nil {
		return fmt.Errorf("shmSize: %w", err)
	}
	return nil
}

// settingsFile is settings.json as it is read: every field optional, and
// one that is missing has its default.
type settingsFile struct {
	Version *int    `json:"version"`
	ShmSize *string `json:"shmSize"`
}

// LoadSettings reads this machine's settings; without the file they are
// the defaults.
func (s Store) LoadSettings() (Settings, error) {
	settings := DefaultSettings()
	data, err := os.ReadFile(s.SettingsFile())
	if errors.Is(err, fs.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	fail := func(err error) (Settings, error) {
		return DefaultSettings(), fmt.Errorf("%s: %w", s.SettingsFile(), err)
	}
	var raw settingsFile
	if err := decodeStrict(data, &raw); err != nil {
		return fail(err)
	}
	switch {
	case raw.Version == nil:
		return fail(errors.New("no version"))
	case *raw.Version != FormatVersion:
		return fail(fmt.Errorf("version %d: this bh reads version %d", *raw.Version, FormatVersion))
	}
	if raw.ShmSize != nil {
		size, err := ParseShmSize(*raw.ShmSize)
		if err != nil {
			return fail(fmt.Errorf("shmSize: %w", err))
		}
		settings.ShmSize = size
	}
	return settings, nil
}

// SaveSettings writes the settings, every one stated, so that the file
// shows what can be edited. The directory is initialized first if need be.
func (s Store) SaveSettings(settings Settings) error {
	if err := settings.validate(); err != nil {
		return err
	}
	if _, err := s.Init(); err != nil {
		return err
	}
	return writeJSON(s.SettingsFile(), struct {
		Version int    `json:"version"`
		ShmSize string `json:"shmSize"`
	}{FormatVersion, FormatSize(settings.ShmSize)})
}
