This Functional options on RegisterWith* is so awesome, probably we can have all RegisterWith* collapsed into Register() with handpicked Functional options based on required features

Strongly agree this is the better shape. Each existing variant is just a preset combination of "request handler / body handler / response handler /
buffered / group / config / onStreamComplete." Functional options dissolve the combinatorial explosion:
up.Register("orange-classify",
    up.WithHandler(requestHandler),
    up.WithMutableBody(bodyHandler),
    up.WithOnStreamComplete(onStreamComplete),
)
Two design questions worth resolving before doing it:
1. Is handler required (positional) or an option? Today every variant requires a HandlerFunc. Making it positional (Register(name, h, opts...))
preserves the invariant cheaply. Making it an option (WithHandler) is more uniform but lets you forget it. I'd keep h positional.
2. Migration strategy. Six call sites (Register, RegisterWithGroup, RegisterWithResponse, RegisterWithBody, RegisterWithMutableBody,
RegisterWithConfig) across the examples. Options:
  - Add the new shape, keep the old as thin shims, delete the shims later.
  - Replace in one sweep (acceptable since this is pre-1.0).
Want me to plan that as the next change? It's a clean refactor and would also let me drop the bufferBody bool field in favor of an option-driven
flag, which matches the spirit of "handpicked features."