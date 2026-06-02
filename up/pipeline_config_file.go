package up

import (
	"context"
	"os"
)

// fileSource implements ConfigSource by reading a file on every Fetch call.
type fileSource struct {
	path string
}

// FileSource returns a ConfigSource that reads path on every Fetch call.
// The file is read fresh each time; no caching. Suitable as the source for
// a polling PipelineConfig.
func FileSource(path string) ConfigSource {
	return &fileSource{path: path}
}

func (f *fileSource) Fetch(_ context.Context) ([]byte, error) {
	return os.ReadFile(f.path)
}
