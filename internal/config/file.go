package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"go.yaml.in/yaml/v3"
)

// maxConfigFileSize bounds how much is read from the configuration file. A configuration is a
// handful of lines; anything larger is a mistake and should not be parsed.
const maxConfigFileSize = 1 << 20

// applyFile overlays a YAML configuration file onto the running configuration.
//
// The file may set any subset of the fields; anything it omits keeps the value it already had.
// A file the caller named explicitly must exist. A file at the default location need not, so LAC
// runs without any configuration at all. Unknown keys are an error rather than a silent no-op,
// because a misspelled security setting that quietly does nothing is the worst kind of typo.
func applyFile(configuration *Config, path string, explicit bool) error {
	contents, err := readConfigFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return nil
		}
		return err
	}

	if err := checkPrivateFile(path); err != nil {
		return err
	}

	// Decode into the current configuration so unset keys keep their resolved defaults.
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)

	if err := decoder.Decode(configuration); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: reading %s: %w", ErrInvalidConfig, path, err)
	}

	return nil
}

func readConfigFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspecting %s: %w", path, err)
	}
	if info.Size() > maxConfigFileSize {
		return nil, fmt.Errorf("%w: %s is %d bytes; a configuration file must be under %d",
			ErrInvalidConfig, path, info.Size(), maxConfigFileSize)
	}

	contents, err := os.ReadFile(path) //nolint:gosec // the operator's own configuration file
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	return contents, nil
}
