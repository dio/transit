module github.com/dio/transit/examples

go 1.26.2

require (
	github.com/dio/transit v0.0.0
	github.com/envoyproxy/envoy/source/extensions/dynamic_modules v0.0.0-20260521055639-0d6e3c60aa55
	github.com/jackc/pgx/v5 v5.9.2
	github.com/stretchr/testify v1.11.1
	go.uber.org/mock v0.6.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/dio/transit => ../
