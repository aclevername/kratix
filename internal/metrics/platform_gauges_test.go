package metrics_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	kratixmetrics "github.com/syntasso/kratix/internal/metrics"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("Platform state gauges", func() {
	var (
		ctx     context.Context
		reader  *sdkmetric.ManualReader
		restore func()
	)

	BeforeEach(func() {
		ctx = context.Background()

		reader = sdkmetric.NewManualReader()
		original := otel.GetMeterProvider()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
		restore = func() {
			otel.SetMeterProvider(original)
		}
	})

	AfterEach(func() {
		if restore != nil {
			restore()
		}
	})

	It("reports promise availability, destination readiness, and work scheduling status", func() {
		scheme := runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())

		availablePromise := &v1alpha1.Promise{
			ObjectMeta: metav1.ObjectMeta{Name: "redis"},
			Status: v1alpha1.PromiseStatus{
				Conditions: []metav1.Condition{{
					Type:               v1alpha1.PromiseAvailableConditionType,
					Status:             metav1.ConditionTrue,
					Reason:             v1alpha1.PromiseAvailableConditionTrueReason,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
		unavailablePromise := &v1alpha1.Promise{
			ObjectMeta: metav1.ObjectMeta{Name: "postgres"},
			Status: v1alpha1.PromiseStatus{
				Conditions: []metav1.Condition{{
					Type:               v1alpha1.PromiseAvailableConditionType,
					Status:             metav1.ConditionFalse,
					Reason:             v1alpha1.PromiseAvailableConditionFalseReason,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}

		readyDestination := &v1alpha1.Destination{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: v1alpha1.DestinationStatus{
				Conditions: []metav1.Condition{{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					Reason:             v1alpha1.DestinationReadyReason,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
		notReadyDestination := &v1alpha1.Destination{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
			Status: v1alpha1.DestinationStatus{
				Conditions: []metav1.Condition{{
					Type:               "Ready",
					Status:             metav1.ConditionFalse,
					Reason:             v1alpha1.DestinationNotReadyReason,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}

		readyWork := workWithReadyCondition("work-1", "redis", "instance-a", metav1.ConditionTrue, "AllWorkplacementsScheduled")
		anotherReadyWork := workWithReadyCondition("work-2", "redis", "instance-b", metav1.ConditionTrue, "AllWorkplacementsScheduled")
		unscheduledWork := workWithReadyCondition("work-3", "redis", "instance-c", metav1.ConditionFalse, "UnscheduledWorkloads")
		misplacedWork := workWithReadyCondition("work-4", "postgres", "", metav1.ConditionFalse, "Misplaced")
		failingWork := workWithReadyCondition("work-5", "postgres", "instance-d", metav1.ConditionFalse, "WorkplacementsFailing")
		pendingWork := workWithReadyCondition("work-6", "postgres", "instance-e", metav1.ConditionUnknown, "Pending")

		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			availablePromise, unavailablePromise,
			readyDestination, notReadyDestination,
			readyWork, anotherReadyWork, unscheduledWork, misplacedWork, failingWork, pendingWork,
		).Build()

		registration, err := kratixmetrics.RegisterPlatformStateGauges(logr.Discard(), fakeClient)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			Expect(registration.Unregister()).To(Succeed())
		}()

		var resourceMetrics metricdata.ResourceMetrics
		Expect(reader.Collect(ctx, &resourceMetrics)).To(Succeed())

		promiseAvailability := gaugeDataPoints(resourceMetrics, kratixmetrics.PromiseAvailableMetric)
		Expect(promiseAvailability).To(HaveLen(2))
		Expect(gaugeValue(promiseAvailability, map[string]string{"promise": "redis"})).To(Equal(int64(1)))
		Expect(gaugeValue(promiseAvailability, map[string]string{"promise": "postgres"})).To(Equal(int64(0)))

		destinationReadiness := gaugeDataPoints(resourceMetrics, kratixmetrics.DestinationReadyMetric)
		Expect(destinationReadiness).To(HaveLen(2))
		Expect(gaugeValue(destinationReadiness, map[string]string{"destination": "worker-1"})).To(Equal(int64(1)))
		Expect(gaugeValue(destinationReadiness, map[string]string{"destination": "worker-2"})).To(Equal(int64(0)))

		works := gaugeDataPoints(resourceMetrics, kratixmetrics.WorksMetric)
		Expect(gaugeValue(works, map[string]string{
			"promise": "redis", "work_type": "resource", "status": kratixmetrics.WorkStatusReady,
		})).To(Equal(int64(2)))
		Expect(gaugeValue(works, map[string]string{
			"promise": "redis", "work_type": "resource", "status": kratixmetrics.WorkStatusUnscheduled,
		})).To(Equal(int64(1)))
		Expect(gaugeValue(works, map[string]string{
			"promise": "postgres", "work_type": "promise", "status": kratixmetrics.WorkStatusMisplaced,
		})).To(Equal(int64(1)))
		Expect(gaugeValue(works, map[string]string{
			"promise": "postgres", "work_type": "resource", "status": kratixmetrics.WorkStatusFailing,
		})).To(Equal(int64(1)))
		Expect(gaugeValue(works, map[string]string{
			"promise": "postgres", "work_type": "resource", "status": kratixmetrics.WorkStatusPending,
		})).To(Equal(int64(1)))
	})

	It("skips observations when listing fails, without erroring the collection", func() {
		emptyScheme := runtime.NewScheme()
		fakeClient := fake.NewClientBuilder().WithScheme(emptyScheme).Build()

		registration, err := kratixmetrics.RegisterPlatformStateGauges(logr.Discard(), fakeClient)
		Expect(err).NotTo(HaveOccurred())
		defer func() {
			Expect(registration.Unregister()).To(Succeed())
		}()

		var resourceMetrics metricdata.ResourceMetrics
		Expect(reader.Collect(ctx, &resourceMetrics)).To(Succeed())

		Expect(gaugeDataPoints(resourceMetrics, kratixmetrics.PromiseAvailableMetric)).To(BeEmpty())
		Expect(gaugeDataPoints(resourceMetrics, kratixmetrics.DestinationReadyMetric)).To(BeEmpty())
		Expect(gaugeDataPoints(resourceMetrics, kratixmetrics.WorksMetric)).To(BeEmpty())
	})
})

func workWithReadyCondition(name, promiseName, resourceName string, status metav1.ConditionStatus, reason string) *v1alpha1.Work {
	return &v1alpha1.Work{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kratix-platform-system"},
		Spec: v1alpha1.WorkSpec{
			PromiseName:  promiseName,
			ResourceName: resourceName,
		},
		Status: v1alpha1.WorkStatus{
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             status,
				Reason:             reason,
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func gaugeDataPoints(resourceMetrics metricdata.ResourceMetrics, metricName string) []metricdata.DataPoint[int64] {
	var dataPoints []metricdata.DataPoint[int64]
	for _, scopeMetrics := range resourceMetrics.ScopeMetrics {
		for _, m := range scopeMetrics.Metrics {
			if m.Name != metricName {
				continue
			}
			gauge, ok := m.Data.(metricdata.Gauge[int64])
			Expect(ok).To(BeTrue(), "expected %s to be an int64 gauge", metricName)
			dataPoints = append(dataPoints, gauge.DataPoints...)
		}
	}
	return dataPoints
}

func gaugeValue(dataPoints []metricdata.DataPoint[int64], expectedAttributes map[string]string) int64 {
	for _, dataPoint := range dataPoints {
		matchesAll := true
		for key, expected := range expectedAttributes {
			value, found := dataPoint.Attributes.Value(attribute.Key(key))
			if !found || value.AsString() != expected {
				matchesAll = false
				break
			}
		}
		if matchesAll {
			return dataPoint.Value
		}
	}
	Fail("no gauge data point found matching the expected attributes")
	return 0
}
