// simulate feeds synthetic request records into the request-ui sink so you
// can see the UI working without Envoy or Docker Compose.
//
// Run (memory mode, no external deps):
//
//	REQUI_ADDR=0.0.0.0:6062 go run ./examples/request-ui/cmd/simulate
//
// Run (postgres mode):
//
//	docker run -d --name requi-pg \
//	  -e POSTGRES_USER=requi -e POSTGRES_PASSWORD=requi -e POSTGRES_DB=requi \
//	  -p 5432:5432 postgres:16-alpine
//
//	REQUI_MODE=postgres \
//	REQUI_DSN=postgres://requi:requi@localhost:5432/requi?sslmode=disable \
//	REQUI_ADDR=0.0.0.0:6062 \
//	go run ./examples/request-ui/cmd/simulate
//
// Then open http://localhost:6062/
package main

import (
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"time"

	requestuisink "github.com/dio/transit/examples/request-ui/sink"
)

func main() {
	if os.Getenv("REQUI_MODE") == "" {
		if err := os.Setenv("REQUI_MODE", "memory"); err != nil {
			fmt.Fprintf(os.Stderr, "simulate: setenv: %v\n", err)
			os.Exit(1)
		}
	}

	s := requestuisink.New()
	s.Start()

	time.Sleep(300 * time.Millisecond)

	fmt.Fprintln(os.Stderr, "[simulate] generating traffic -- open http://localhost:6062/")

	go generate(s)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Fprintln(os.Stderr, "[simulate] stopping")
}

func generate(s *requestuisink.Sink) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	methods := []string{"GET", "POST", "GET", "GET", "PUT", "DELETE", "GET"}
	paths := []string{
		"/v1/chat/completions",
		"/v1/messages",
		"/api/users",
		"/api/users/42",
		"/api/orders",
		"/api/health",
		"/v1/embeddings",
		"/api/slow-endpoint",
		"/api/error-endpoint",
	}

	counter := int64(0)
	for {
		counter++
		r := syntheticRecord(rng, counter, methods, paths)
		s.Send(r)
		time.Sleep(time.Duration(80+rng.Intn(80)) * time.Millisecond)
	}
}

type scenario int

const (
	scOK             scenario = iota
	scClientError             // upstream 4xx
	scServerError             // upstream 5xx
	scTimeout                 // Envoy UT flag
	scUpstreamReset           // UF flag
	scCircuitBreaker          // UO flag
	scNoRoute                 // NR flag
	scDownstreamDC            // DC flag (client disconnect) -- not an upstream error
	scSlow                    // normal 200 but high duration
	numScenarios
)

func syntheticRecord(rng *rand.Rand, id int64, methods, paths []string) *requestuisink.Record {
	weights := []int{50, 8, 8, 6, 5, 4, 3, 4, 12}
	sc := scenario(weighted(rng, weights))

	method := methods[rng.Intn(len(methods))]
	path := paths[rng.Intn(len(paths))]
	reqID := fmt.Sprintf("req-%06d", id)
	host := []string{"api.example.com", "gateway.example.com", "proxy.internal"}[rng.Intn(3)]
	upstream := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"}[rng.Intn(3)]

	r := &requestuisink.Record{
		RequestID:       reqID,
		Method:          method,
		Path:            path,
		Host:            host,
		UpstreamAddress: upstream,
		Timestamp:       time.Now().UTC().Format(time.RFC3339),
		RequestHeaders:  sampleRequestHeaders(method, host, reqID),
	}

	switch sc {
	case scOK:
		r.ResponseCode = 200
		r.UpstreamStatus = "200"
		r.DurationMs = float64(5 + rng.Intn(150))
		r.ResponseCodeDetails = "via_upstream"
		r.ResponseHeaders = `[[":status","200"],["content-type","application/json"]]`
		r.ResponseBody = `{"ok":true}`

	case scClientError:
		code := []int{400, 401, 403, 404, 422}[rng.Intn(5)]
		r.ResponseCode = float64(code)
		r.UpstreamStatus = fmt.Sprintf("%d", code)
		r.DurationMs = float64(5 + rng.Intn(80))
		r.ResponseCodeDetails = "via_upstream"
		r.ResponseHeaders = fmt.Sprintf(`[[":status","%d"],["content-type","application/json"]]`, code)

	case scServerError:
		r.ResponseCode = 503
		r.UpstreamStatus = "503"
		r.DurationMs = float64(10 + rng.Intn(200))
		r.ResponseCodeDetails = "via_upstream"
		r.ResponseHeaders = `[[":status","503"],["content-type","application/json"]]`
		r.HasError = true

	case scTimeout:
		r.ResponseCode = 504
		r.UpstreamStatus = "504"
		r.DurationMs = float64(5000 + rng.Intn(2000))
		r.ResponseFlags = "UT"
		r.ResponseCodeDetails = "response_timeout"
		r.ErrorDetails = "response_timeout"
		r.HasError = true

	case scUpstreamReset:
		r.ResponseCode = 503
		r.UpstreamStatus = "503"
		r.DurationMs = float64(20 + rng.Intn(100))
		r.ResponseFlags = "UF"
		r.ResponseCodeDetails = "upstream_reset_before_response_started{connection_failure}"
		r.ErrorDetails = "upstream_reset_before_response_started{connection_failure}"
		r.HasError = true

	case scCircuitBreaker:
		r.ResponseCode = 503
		r.UpstreamStatus = "503"
		r.DurationMs = float64(1 + rng.Intn(5))
		r.ResponseFlags = "UO"
		r.ResponseCodeDetails = "upstream_overflow"
		r.ErrorDetails = "upstream_overflow"
		r.HasError = true

	case scNoRoute:
		r.ResponseCode = 404
		r.UpstreamStatus = "404"
		r.DurationMs = float64(1 + rng.Intn(3))
		r.ResponseFlags = "NR"
		r.ResponseCodeDetails = "route_not_found"
		r.UpstreamAddress = ""
		r.HasError = true

	case scDownstreamDC:
		r.ResponseCode = 0
		r.DurationMs = float64(50 + rng.Intn(3000))
		r.ResponseFlags = "DC"
		r.ResponseCodeDetails = "downstream_remote_disconnect"

	case scSlow:
		r.ResponseCode = 200
		r.UpstreamStatus = "200"
		r.DurationMs = float64(600 + rng.Intn(2000))
		r.ResponseCodeDetails = "via_upstream"
		r.ResponseHeaders = `[[":status","200"],["content-type","application/json"]]`
	}

	r.RequestSizeBytes = float64(100 + rng.Intn(4096))
	r.ResponseSizeBytes = float64(50 + rng.Intn(2048))

	if r.HasError && rng.Intn(4) == 0 {
		r.UpstreamFailure = []string{
			"TLS_error:268435581:SSL routines:OPENSSL_internal:CERTIFICATE_VERIFY_FAILED",
			"TLS_error:167773206:SSL routines:OPENSSL_internal:HANDSHAKE_FAILURE_ON_NEW_SESSION",
			"",
		}[rng.Intn(3)]
	}

	if rng.Intn(3) != 0 {
		r.TraceID = fmt.Sprintf("%032x", rng.Int63())
		r.SpanID = fmt.Sprintf("%016x", rng.Int63())
	}

	return r
}

func sampleRequestHeaders(method, host, reqID string) string {
	return fmt.Sprintf(
		`[["x-request-id","%s"],[":method","%s"],[":authority","%s"],["content-type","application/json"],["user-agent","simulate/1.0"]]`,
		reqID, method, host,
	)
}

func weighted(rng *rand.Rand, weights []int) int {
	total := 0
	for _, w := range weights {
		total += w
	}
	n := rng.Intn(total)
	for i, w := range weights {
		n -= w
		if n < 0 {
			return i
		}
	}
	return len(weights) - 1
}
