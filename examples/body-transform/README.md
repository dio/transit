# body-transform

Demonstrates `up.RegisterWithMutableBody`. The filter reads the complete
request body, renames the JSON field `message` to `text`, and replaces the
body before forwarding.

Non-JSON bodies pass through unchanged.

## Build and run

    make build
    make run

Then:

    curl -s -X POST http://localhost:10000/ \
      -H 'content-type: application/json' \
      -d '{"message":"hello"}' | cat
    # {"text":"hello"}
