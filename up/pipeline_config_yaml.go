package up

import "gopkg.in/yaml.v3"

// YAMLDecoder[T] decodes YAML bytes into T using gopkg.in/yaml.v3.
func YAMLDecoder[T any]() ConfigDecoder[T] {
	return func(data []byte) (T, error) {
		var v T
		if err := yaml.Unmarshal(data, &v); err != nil {
			return v, err
		}
		return v, nil
	}
}
