package config

import (
	"log/slog"
	"os"
)

// MustReload discards the cached pipeline so the next Get call rebuilds it
// from the current environment. Test-only; not safe to call concurrently with Get.
// Sets a default logger if none has been configured yet.
func MustReload() {
	globalMu.Lock()
	globalPipeline = nil
	if globalLogger == nil {
		globalLogger = slog.Default()
	}
	globalMu.Unlock()
}

// LoadFile parses and validates the config at path. Test-only.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data)
}
