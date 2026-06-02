package up

import (
	"context"
	"os"
)

// NewFileConfig returns a PipelineConfig backed by a file path.
// The file is read fresh on every poll tick; path is evaluated at fetch time.
func NewFileConfig[T any](path string, dec ConfigDecoder[T], opts PollOptions) *PipelineConfig[T] {
	src := ConfigSource(func(_ context.Context) ([]byte, error) {
		return os.ReadFile(path)
	})
	return NewPollingConfig(src, dec, opts)
}
