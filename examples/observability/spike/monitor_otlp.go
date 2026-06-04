// Monitor OTLP metrics, logs, and traces for investigation.
// Run alongside run_envoy.sh to see what telemetry is being sent.
//
// Usage:
//
//	go build -o otel-monitor ./monitor_otlp.go
//	./otel-monitor                          # only dynamicmodulescustom.* metrics
//	./otel-monitor -filter ""               # all metrics
//	./otel-monitor -traces -logs            # also print traces and logs
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"

	otlpcollectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	otlpcollectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	otlpcollectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"
)

var (
	metricsFilter = flag.String("filter", "dynamicmodulescustom", "show only metrics whose name contains this substring (empty = all)")
	showTraces    = flag.Bool("traces", false, "print trace spans")
	showLogs      = flag.Bool("logs", false, "print log records")
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
	if !*showTraces {
		return &otlpcollectortrace.ExportTraceServiceResponse{}, nil
	}
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
	printed := 0
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, metric := range sm.Metrics {
				if *metricsFilter != "" && !strings.Contains(metric.Name, *metricsFilter) {
					continue
				}
				printed++
				fmt.Printf("[METRICS] %s\n", metric.Name)
				printMetricData(metric)
			}
		}
	}
	if printed > 0 {
		fmt.Printf("[METRICS] matched %d metric(s)\n\n", printed)
	}
	return &otlpcollectormetrics.ExportMetricsServiceResponse{}, nil
}

func (l *logsSvc) Export(ctx context.Context, req *otlpcollectorlogs.ExportLogsServiceRequest) (*otlpcollectorlogs.ExportLogsServiceResponse, error) {
	if !*showLogs {
		return &otlpcollectorlogs.ExportLogsServiceResponse{}, nil
	}
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
	flag.Parse()

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
	if *metricsFilter != "" {
		fmt.Printf("metrics filter: %q (use -filter \"\" to see all)\n", *metricsFilter)
	} else {
		fmt.Println("metrics filter: none (all metrics)")
	}
	fmt.Printf("traces: %v  logs: %v\n\n", *showTraces, *showLogs)

	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
