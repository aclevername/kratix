package telemetry

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	WorkflowExecutionsMetric = "kratix_workflow_executions_total"
	WorkflowDurationMetric   = "kratix_workflow_duration_seconds"
	WorkflowResultSuccess    = "success"
	WorkflowResultFailure    = "failure"
)

var (
	workflowExecutionCounter  metric.Int64Counter
	workflowDurationHistogram metric.Float64Histogram
	workflowInstrumentsOnce   sync.Once
	errWorkflowInstruments    error
)

// WorkflowAttributes builds OpenTelemetry attributes describing a workflow pipeline execution.
func WorkflowAttributes(workflowType, action, promise, resource, pipeline string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	if workflowType != "" {
		attrs = append(attrs, attribute.String("workflow_type", workflowType))
	}
	if action != "" {
		attrs = append(attrs, attribute.String("action", action))
	}
	if promise != "" {
		attrs = append(attrs, attribute.String("promise", promise))
	}
	if resource != "" {
		attrs = append(attrs, attribute.String("resource", resource))
	}
	if pipeline != "" {
		attrs = append(attrs, attribute.String("pipeline", pipeline))
	}
	return attrs
}

// RecordWorkflowExecution records a completed workflow pipeline execution. The duration is
// only recorded when known (greater than zero); the execution is always counted.
func RecordWorkflowExecution(ctx context.Context, result string, duration time.Duration, attrs ...attribute.KeyValue) {
	workflowInstrumentsOnce.Do(func() {
		meter := otel.Meter(instrumentationName)
		workflowExecutionCounter, errWorkflowInstruments = meter.Int64Counter(
			WorkflowExecutionsMetric,
			metric.WithDescription("Total number of completed workflow pipeline executions"),
		)
		if errWorkflowInstruments != nil {
			return
		}
		workflowDurationHistogram, errWorkflowInstruments = meter.Float64Histogram(
			WorkflowDurationMetric,
			metric.WithDescription("Duration of workflow pipeline executions in seconds"),
			metric.WithUnit("s"),
			metric.WithExplicitBucketBoundaries(1, 5, 10, 30, 60, 120, 300, 600, 1800, 3600),
		)
	})
	if errWorkflowInstruments != nil {
		return
	}

	allAttrs := append([]attribute.KeyValue{attribute.String("result", result)}, attrs...)
	workflowExecutionCounter.Add(ctx, 1, metric.WithAttributes(allAttrs...))
	if duration > 0 {
		workflowDurationHistogram.Record(ctx, duration.Seconds(), metric.WithAttributes(allAttrs...))
	}
}

// ResetWorkflowMetricsForTest clears cached metric state to allow tests to install a fresh meter provider.
func ResetWorkflowMetricsForTest() {
	workflowExecutionCounter = nil
	workflowDurationHistogram = nil
	errWorkflowInstruments = nil
	workflowInstrumentsOnce = sync.Once{}
}
