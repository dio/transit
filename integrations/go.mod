module github.com/dio/transit/integrations

go 1.26.2

require (
	github.com/dio/gateway-pairs v0.2.1
	github.com/dio/transit v0.0.0-20260524011848-c4ee1587ea44
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/dio/sh v0.0.2 // indirect
	github.com/envoyproxy/envoy/source/extensions/dynamic_modules v0.0.0-20260521055639-0d6e3c60aa55 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

require (
	github.com/coder/websocket v1.8.14
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/dio/transit => ..
