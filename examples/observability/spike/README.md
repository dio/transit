# Observability Example Investigation Tools

This directory contains scripts to investigate and debug the observability example's OTLP integration.

## Investigation Status

**Current State:**
- ✅ Tracing works (OTLP traces exported successfully)
- ✅ Logging works (OTLP access logs exported successfully)
- ❓ Metrics incomplete (Transit SDK custom metrics not exported via OTLP)

## Running the Investigation

Terminal 1 - Start OTLP monitor:
```bash
cd examples/observability/spike
go run monitor_otlp.go
```

Terminal 2 - Start Envoy and backend:
```bash
cd examples/observability/spike
./run_envoy.sh
```

Terminal 3 - Send test requests:
```bash
# Simple request
curl -i http://127.0.0.1:9090/

# Request with model header
curl -i -H "x-model: claude-opus" http://127.0.0.1:9090/test

# Check Envoy stats
curl http://127.0.0.1:9901/stats | grep observability
curl http://127.0.0.1:9901/stats/prometheus | grep observability
```

## Key Findings

### Metrics Issue
The observability filter defines custom counters via the Transit SDK:
```go
requestsID, err := h.DefineCounter("observability_requests_total")
responsesID, err := h.DefineCounter("observability_responses_total")
```

These counters are incremented in the filter but are **not being exported via OTLP**. 

**Possible causes:**
1. Transit SDK metrics may not be integrated with Envoy's stats system
2. Stats sink may only export Envoy's native metrics, not dynamic module metrics
3. Additional configuration needed to export custom metrics

**Investigation needed:**
- Check Transit SDK documentation on metric export
- Review how other dynamic modules export metrics
- Verify if metrics are recorded in Envoy's `/stats` endpoint
- Check if there's an explicit OTLP export mechanism for SDK metrics

## Files

- `run_envoy.sh` - Start Envoy with observability filter on fixed ports
- `monitor_otlp.go` - Monitor and log all OTLP telemetry received
- `README.md` - This file

## Next Steps

1. Run the investigation to see if metrics appear in `/stats` but not OTLP
2. If metrics are in `/stats`, investigate stats sink export mechanism
3. If metrics are not in `/stats`, check Transit SDK metric API design
4. Consider whether custom metric export is intended or needs to be implemented separately
