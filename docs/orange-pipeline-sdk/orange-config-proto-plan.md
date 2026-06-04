# Orange Config Proto Plan

Status: planning note. Do not implement generation yet.

## Goal

Make Orange config easy to send over the wire while keeping the YAML/JSON
Schema config shape readable:

```yaml
llm:
  providers: ...
  models: ...
mcp:
  routes: ...
```

The wire type should cover both LLM and MCP config so control-plane delivery
does not need separate channels for `llm` and `mcp`.

## Viability

Building a `.proto` from `examples/orange/internal/config/config.schema.json`
is viable, but not as a blind one-shot conversion.

JSON Schema expresses validation rules that protobuf does not carry natively:

- required object properties;
- URI and `env://...` patterns;
- `if` / `then` constraints, such as `bearer` requiring `secret_ref`;
- `additionalProperties: false`;
- minimum object sizes.

Proto is a good wire shape. JSON Schema should remain the validation source
until we add a second validation layer, such as protovalidate.

## Proposed Source Of Truth

For the next slice, keep `config.schema.json` as the human-authored source of
truth and add a hand-authored proto that mirrors it. Add parity tests so schema
and proto do not drift.

Do not generate production Go structs from the proto yet. The existing
`config.Config` type is already wired through the Orange pipeline. Add explicit
conversion functions:

```go
func ToProto(*Config) (*configv1.OrangeConfig, error)
func FromProto(*configv1.OrangeConfig) (*Config, error)
```

Then run the normal `Load` validation path after `FromProto` by round-tripping
through the same typed config rules, or by adding a shared `Validate(*Config)`
helper.

## File Layout

Because the repo does not currently have a root proto convention, keep this
example-local at first:

```text
examples/orange/internal/config/proto/orange_config.proto
examples/orange/internal/config/proto/README.md
examples/orange/internal/config/proto/compat_test.go
```

If this graduates beyond the Orange example, move it to a repo-level
`proto/orange/config/v1/` package with a proper generation target.

## Proto Shape

Initial proto package:

```proto
syntax = "proto3";

package orange.config.v1;

option go_package = "github.com/dio/transit/examples/orange/internal/config/proto/configv1";

message OrangeConfig {
  LLMConfig llm = 1;
  MCPConfig mcp = 2;
}

message LLMConfig {
  map<string, Provider> providers = 1;
  map<string, ModelEntry> models = 2;
}

message Provider {
  string kind = 1;
  string backend_schema = 2;
  string endpoint = 3;
  optional string path_prefix = 4;
  map<string, string> extra = 5;
  Auth auth = 6;
}

message Auth {
  string type = 1;
  string secret_ref = 2;
}

message ModelEntry {
  string provider = 1;
  string name = 2;
  google.protobuf.Struct metadata = 3;
}

message MCPConfig {
  map<string, MCPRoute> routes = 1;
}

message MCPRoute {
  map<string, MCPBackend> backends = 1;
}

message MCPBackend {
  string cluster = 1;
  string endpoint = 2;
  string credential_ref = 3;
  MCPToolSelector tools = 4;
}

message MCPToolSelector {
  repeated string include = 1;
  repeated string include_regex = 2;
  repeated string exclude = 3;
  repeated string exclude_regex = 4;
}
```

Use strings for `kind`, `backend_schema`, and auth `type` in v1. They are enum
strings in JSON Schema, but keeping them as strings avoids a migration every
time a translator/auth handler is added. Validation remains in the config
package.

Use `google.protobuf.Struct` for model metadata because the schema intentionally
allows provider-defined metadata and pricing shapes.

## Build Tooling

Do not add repo-wide `buf` yet unless another package needs proto generation.
For the first implementation, use the smallest local target that works:

```makefile
proto:
	protoc --go_out=. --go_opt=paths=source_relative \
	  examples/orange/internal/config/proto/orange_config.proto
```

If `protoc` availability becomes a developer friction point, add a tool module
or switch to Buf in a follow-up.

## Compatibility Tests

Add tests that prove the proto mirrors the schema:

1. Load `examples/orange/orange.yaml` through `config.LoadFile`.
2. Convert `Config -> proto -> Config`.
3. Assert key LLM and MCP fields survive:
   - provider endpoint/auth/path_prefix/extra;
   - model metadata;
   - MCP route/backend endpoint/credential/tool selectors.
4. Validate the resulting config with existing semantic checks.
5. Add a golden JSON rendering of the proto only if we need stable REST
   transport later.

Also add negative tests at the config layer, not the proto layer:

- missing `llm`;
- missing provider ref;
- invalid auth type;
- GitHub MCP credential ref missing env var.

## Schema-To-Proto Generation Later

A generator can be added after the hand-authored proto proves useful. The safe
scope for generation is:

- object definitions to messages;
- string maps from `additionalProperties: { "$ref": ... }`;
- repeated strings for array-of-string fields;
- `google.protobuf.Struct` for open objects.

The generator should not try to encode validation rules into proto comments and
pretend they are enforced. It should emit comments that point back to
`config.schema.json` and rely on tests to detect drift.

## Exit Criteria

This plan is ready to implement when:

- `llm` + `mcp` root shape is settled in `config.schema.json`;
- Orange demo can run from YAML with the new shape;
- we know whether the first wire consumer wants binary protobuf, protobuf JSON,
  or both.

Implementation is complete when:

- proto file exists and generated Go is checked in or generated by a documented
  target;
- conversion tests pass;
- YAML load path remains unchanged for local demo users;
- no config validation rule is lost compared with `config.schema.json`.
