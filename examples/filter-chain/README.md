# filter-chain

Demonstrates `up.Chain` and `up.Middleware` to compose multiple request-handling
behaviors into a single filter.

Three middleware are chained:
1. **WithLogging** — logs every request at Info level
2. **WithRequiredHeader** — rejects requests missing `x-api-key` with 401
3. **WithStampHeader** — adds `x-filtered: true` to every forwarded request

## Build and run

    make build
    make run

Then:

    # passes (x-api-key present)
    curl -s http://localhost:10000/ -H 'x-api-key: secret'

    # rejected (missing x-api-key)
    curl -s http://localhost:10000/
    # {"error":"missing required header"}
