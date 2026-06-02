package up

import (
	"gopkg.in/yaml.v3"
)

// yamlDecoder is the implementation of ConfigDecoder[T] for YAML.
type yamlDecoder[T any] struct{}

func (d yamlDecoder[T]) Decode(data []byte) (T, error) {
	var v T
	if err := yaml.Unmarshal(data, &v); err != nil {
		return v, err
	}
	return v, nil
}

// YAMLDecoder[T] decodes YAML bytes into T using gopkg.in/yaml.v3.
func YAMLDecoder[T any]() ConfigDecoder[T] {
	return yamlDecoder[T]{}
}
