// Monitor OTLP metrics, logs, and traces for investigation.
// Run alongside run_envoy.sh to see what telemetry is being sent.
//
// Usage:
//   go build -o otel-monitor ./monitor_otlp.go
//   ./otel-monitor
//
package main

import (
	"context"
	"fmt"
	"net"
	"sync"

	otlpcollectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcollectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
)

type traceSvc struct {
	otlpcollectortrace.UnimplementedTraceServiceServer
	mu sync.Mutex
}

type metricsSvc struct {
	otlpcollectormetrics.UnimplementedMetricsServiceServer
	mu sync.Mutex
}

type logsSvc struct {
	otlpcollectorlogs.UnimplementedLogsServiceServer
	mu sync.Mutex
}

func (t *traceSvc) Export(ctx context.Context, req *otlpcollectortrace.ExportTraceServiceRequest) (*otlpcollectortrace.ExportTraceServiceResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				count++
				fmt.Printf("[TRACE] Span: %x (op=%s)\n", span.TraceId, span.Name)
				for _, attr := range span.Attributes {
					fmt.Printf("        attr: %s = %v\n", attr.Key, attr.Value)
				}
			}
		}
	}
	fmt.Printf("[TRACE] Exported %d spans\n\n", count)
	return &otlpcollectortrace.ExportTraceServiceResponse{}, nil
}

func (m *metricsSvc) Export(ctx context.Context, req *otlpcollectormetrics.ExportMetricsServiceRequest) (*otlpcollectormetrics.ExportMetricsServiceResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.Metrics {
				count++
				fmt.Printf("[METRICS] Metric: %s\n", metric.Name)
				printMetricData(metric)
			}
		}
	}
	fmt.Printf("[METRICS] Exported %d metrics\n\n", count)
	return &otlpcollectormetrics.ExportMetricsServiceResponse{}, nil
}

func (l *logsSvc) Export(ctx context.Context, req *otlpcollectorlogs.ExportLogsServiceRequest) (*otlpcollectorlogs.ExportLogsServiceResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, rl := range req.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				count++
				fmt.Printf("[LOGS] LogRecord: %s\n", lr.Body)
				for _, attr := range lr.Attributes {
					fmt.Printf("       attr: %s = %v\n", attr.Key, attr.Value)
				}
			}
		}
	}
	fmt.Printf("[LOGS] Exported %d log records\n\n", count)
	return &otlpcollectorlogs.ExportLogsServiceResponse{}, nil
}

func printMetricData(metric *otlpmetrics.Metric) {
	if sum := metric.GetSum(); sum != nil {
		for _, dp := range sum.DataPoints {
			if dp.Value != nil {
				fmt.Printf("        value: %v\n", dp.Value)
			}
			for _, attr := range dp.Attributes {
				fmt.Printf("        attr: %s = %v\n", attr.Key, attr.Value)
			}
		}
	} else if gauge := metric.GetGauge(); gauge != nil {
		for _, dp := range gauge.DataPoints {
			if dp.Value != nil {
				fmt.Printf("        value: %v\n", dp.Value)
			}
			for _, attr := range dp.Attributes {
				fmt.Printf("        attr: %s = %v\n", attr.Key, attr.Value)
			}
		}
	} else if histogram := metric.GetHistogram(); histogram != nil {
		for _, dp := range histogram.DataPoints {
			fmt.Printf("        count: %d, sum: %v\n", dp.Count, dp.Sum)
		}
	}
}

func main() {
	lis, err := net.Listen("tcp", "127.0.0.1:9093")
	if err != nil {
		fmt.Printf("Failed to listen: %v\n", err)
		panic(err)
	}

	srv := grpc.NewServer()
	otlpcollectortrace.RegisterTraceServiceServer(srv, &traceSvc{})
	otlpcollectormetrics.RegisterMetricsServiceServer(srv, &metricsSvc{})
	otlpcollectorlogs.RegisterLogsServiceServer(srv, &logsSvc{})

	fmt.Println("OTLP Monitor listening on 127.0.0.1:9093")
	fmt.Println("Waiting for telemetry from Envoy...")
	fmt.Println("")

	srv.Serve(lis)
}
