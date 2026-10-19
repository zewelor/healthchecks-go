package healthchecks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

func ReadConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open configuration file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read configuration file: %w", err)
	}
	if len(data) > MaxConfigBytes {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	return data, nil
}

type ConfigWatcher struct {
	path   string
	engine *Engine
	lookup func(string) (string, bool)
	last   []byte
}

func NewConfigWatcher(path string, e *Engine, lookup func(string) (string, bool)) *ConfigWatcher {
	return &ConfigWatcher{path: path, engine: e, lookup: lookup}
}

// Check follows atomic file and ConfigMap symlink replacement. Call it serially.
func (w *ConfigWatcher) Check() (bool, error) {
	data, err := ReadConfigFile(w.path)
	if err != nil {
		w.engine.SetReloadError(err.Error())
		return false, err
	}
	if bytes.Equal(data, w.last) {
		w.engine.SetReloadError("")
		return false, nil
	}
	c, err := ParseConfig(data, w.lookup)
	if err != nil {
		w.engine.SetReloadError(err.Error())
		return false, err
	}
	if err := w.engine.Reload(c); err != nil {
		w.engine.SetReloadError(err.Error())
		return false, err
	}
	w.last = data
	return true, nil
}

func (w *ConfigWatcher) Run(ctx context.Context, report func(error)) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	lastMessage := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := w.Check()
			message := ""
			if err != nil {
				message = err.Error()
			}
			if changed || message != lastMessage {
				report(err)
			}
			lastMessage = message
		}
	}
}
