# jwt-callout

Demonstrates `up.Register` with `w.HTTPCallout` for JWT validation via an
upstream introspection service.

The filter reads `Authorization: Bearer <token>`, calls the
`token-introspection` cluster at `POST /introspect`, and:
- Sets `x-jwt-sub: <sub>` and forwards the request if `active == true`
- Returns 401 if the token is inactive or the callout fails
- Returns 401 if the `Authorization` header is missing or not Bearer

## Build and run

    make build
    make run

Then:

    curl -s http://localhost:10000/ -H 'Authorization: Bearer mytoken'
