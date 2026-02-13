package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeExecRunner struct {
	stdout string
	stderr string
	err    error
	calls  int
}

func (f *fakeExecRunner) Exec(context.Context, string, string, string, []string) (string, string, error) {
	f.calls++
	return f.stdout, f.stderr, f.err
}

func TestDecodeManifestDocumentsMultiDocumentYAML(t *testing.T) {
	t.Parallel()

	manifest := []byte(`
apiVersion: platform.kratix.io/v1alpha1
kind: Promise
metadata:
  name: redis
  labels:
    kratix.io/promise-version: v1.2.3
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: redis-settings
  namespace: default
data:
  value: "true"
`)

	objects, err := decodeManifestDocuments(manifest)
	if err != nil {
		t.Fatalf("expected multi-doc manifest to decode successfully, got error: %v", err)
	}

	if len(objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objects))
	}

	if objects[0].GetKind() != "Promise" {
		t.Fatalf("expected first object kind Promise, got %s", objects[0].GetKind())
	}

	if objects[1].GetKind() != "ConfigMap" {
		t.Fatalf("expected second object kind ConfigMap, got %s", objects[1].GetKind())
	}
}

func TestValidateVersionSetsBlankPromiseReleaseVersion(t *testing.T) {
	t.Parallel()

	k8sClient := newPromiseReleaseUnitTestClient(t)
	reconciler := &PromiseReleaseReconciler{
		Client: k8sClient,
	}

	promiseRelease := &v1alpha1.PromiseRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
		},
		Spec: v1alpha1.PromiseReleaseSpec{
			Version: "",
			SourceRef: v1alpha1.SourceRef{
				Type: v1alpha1.TypeHTTP,
				URL:  "https://example.com/promise.yaml",
			},
		},
	}
	if err := k8sClient.Create(context.Background(), promiseRelease); err != nil {
		t.Fatalf("failed to create PromiseRelease: %v", err)
	}

	promise := &v1alpha1.Promise{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
			Labels: map[string]string{
				v1alpha1.PromiseVersionLabel: "v1.2.3",
			},
		},
	}

	updated, err := reconciler.validateVersion(opts{
		ctx:    context.Background(),
		client: k8sClient,
		logger: logr.Discard(),
	}, promiseRelease, promise)
	if err != nil {
		t.Fatalf("expected version validation to update PromiseRelease version, got error: %v", err)
	}
	if !updated {
		t.Fatalf("expected version validation to update PromiseRelease version")
	}

	updatedPromiseRelease := &v1alpha1.PromiseRelease{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: promiseRelease.Name}, updatedPromiseRelease); err != nil {
		t.Fatalf("failed to fetch updated PromiseRelease: %v", err)
	}

	if updatedPromiseRelease.Spec.Version != "v1.2.3" {
		t.Fatalf("expected PromiseRelease version to be set to v1.2.3, got %s", updatedPromiseRelease.Spec.Version)
	}
}

func TestValidateVersionMismatchSetsStatusCondition(t *testing.T) {
	t.Parallel()

	k8sClient := newPromiseReleaseUnitTestClient(t)
	reconciler := &PromiseReleaseReconciler{
		Client: k8sClient,
	}

	promiseRelease := &v1alpha1.PromiseRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
		},
		Spec: v1alpha1.PromiseReleaseSpec{
			Version: "v1.0.0",
			SourceRef: v1alpha1.SourceRef{
				Type: v1alpha1.TypeHTTP,
				URL:  "https://example.com/promise.yaml",
			},
		},
	}
	if err := k8sClient.Create(context.Background(), promiseRelease); err != nil {
		t.Fatalf("failed to create PromiseRelease: %v", err)
	}

	promise := &v1alpha1.Promise{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
			Labels: map[string]string{
				v1alpha1.PromiseVersionLabel: "v2.0.0",
			},
		},
	}

	updated, err := reconciler.validateVersion(opts{
		ctx:    context.Background(),
		client: k8sClient,
		logger: logr.Discard(),
	}, promiseRelease, promise)
	if err == nil {
		t.Fatalf("expected version mismatch to return an error")
	}
	if updated {
		t.Fatalf("expected version mismatch not to update PromiseRelease version")
	}

	updatedPromiseRelease := &v1alpha1.PromiseRelease{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: promiseRelease.Name}, updatedPromiseRelease); err != nil {
		t.Fatalf("failed to fetch updated PromiseRelease: %v", err)
	}

	if len(updatedPromiseRelease.Status.Conditions) != 1 {
		t.Fatalf("expected one status condition, got %d", len(updatedPromiseRelease.Status.Conditions))
	}

	condition := updatedPromiseRelease.Status.Conditions[0]
	if condition.Reason != conditionReasonVersionMismatch {
		t.Fatalf("expected reason %s, got %s", conditionReasonVersionMismatch, condition.Reason)
	}
	if condition.Status != metav1.ConditionFalse {
		t.Fatalf("expected condition status False, got %s", condition.Status)
	}
}

func TestReconcileOCISourceManifestNotFoundSetsConditionAndDeletesPod(t *testing.T) {
	t.Parallel()

	k8sClient := newPromiseReleaseUnitTestClient(t)
	execRunner := &fakeExecRunner{
		stderr: "cat: /manifest/promise.yaml: No such file or directory",
		err:    fmt.Errorf("exit status 1"),
	}
	reconciler := &PromiseReleaseReconciler{
		Client:     k8sClient,
		Scheme:     k8sClient.Scheme(),
		ExecRunner: execRunner,
	}

	promiseRelease := &v1alpha1.PromiseRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
		},
		Spec: v1alpha1.PromiseReleaseSpec{
			Version: "v1.2.3",
			SourceRef: v1alpha1.SourceRef{
				Type:  v1alpha1.TypeOCI,
				Image: "ghcr.io/org/promise:1.2.3",
			},
		},
	}
	if err := k8sClient.Create(context.Background(), promiseRelease); err != nil {
		t.Fatalf("failed to create PromiseRelease: %v", err)
	}

	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: promiseRelease.Name}, promiseRelease); err != nil {
		t.Fatalf("failed to fetch PromiseRelease: %v", err)
	}

	helperPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ociHelperPodName(promiseRelease),
			Namespace: ociHelperPodNamespace(promiseRelease),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  ociHelperContainerName,
					Image: promiseRelease.Spec.SourceRef.Image,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
	if err := k8sClient.Create(context.Background(), helperPod); err != nil {
		t.Fatalf("failed to create helper pod: %v", err)
	}

	// Reconcile #1 adds the finalizer.
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: promiseRelease.Name}}); err != nil {
		t.Fatalf("failed during finalizer reconcile: %v", err)
	}

	// Reconcile #2 executes OCI flow and should fail on missing manifest.
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: promiseRelease.Name}}); err == nil {
		t.Fatalf("expected manifest-not-found reconcile to return an error")
	}

	updatedPromiseRelease := &v1alpha1.PromiseRelease{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: promiseRelease.Name}, updatedPromiseRelease); err != nil {
		t.Fatalf("failed to fetch PromiseRelease after reconcile: %v", err)
	}

	if len(updatedPromiseRelease.Status.Conditions) != 1 {
		t.Fatalf("expected one status condition, got %d", len(updatedPromiseRelease.Status.Conditions))
	}
	if updatedPromiseRelease.Status.Conditions[0].Reason != conditionReasonManifestMissing {
		t.Fatalf("expected reason %s, got %s", conditionReasonManifestMissing, updatedPromiseRelease.Status.Conditions[0].Reason)
	}

	podAfterReconcile := &corev1.Pod{}
	err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      helperPod.Name,
		Namespace: helperPod.Namespace,
	}, podAfterReconcile)
	if err == nil {
		t.Fatalf("expected helper pod to be deleted after terminal failure")
	}
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("expected helper pod get to return not found, got %v", err)
	}
}

func TestReconcileOCIHelperPodIsIdempotent(t *testing.T) {
	t.Parallel()

	k8sClient := newPromiseReleaseUnitTestClient(t)
	reconciler := &PromiseReleaseReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
	}

	promiseRelease := &v1alpha1.PromiseRelease{
		ObjectMeta: metav1.ObjectMeta{
			Name: "redis",
		},
		Spec: v1alpha1.PromiseReleaseSpec{
			Version: "v1.2.3",
			SourceRef: v1alpha1.SourceRef{
				Type:  v1alpha1.TypeOCI,
				Image: "ghcr.io/org/promise:1.2.3",
			},
		},
	}
	if err := k8sClient.Create(context.Background(), promiseRelease); err != nil {
		t.Fatalf("failed to create PromiseRelease: %v", err)
	}

	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: promiseRelease.Name}, promiseRelease); err != nil {
		t.Fatalf("failed to fetch PromiseRelease: %v", err)
	}

	o := opts{
		ctx:    context.Background(),
		client: k8sClient,
		logger: logr.Discard(),
	}

	firstPod, created, err := reconciler.reconcileOCIHelperPod(o, promiseRelease)
	if err != nil {
		t.Fatalf("expected helper pod reconcile to succeed, got error: %v", err)
	}
	if !created {
		t.Fatalf("expected first helper pod reconcile to create pod")
	}

	secondPod, created, err := reconciler.reconcileOCIHelperPod(o, promiseRelease)
	if err != nil {
		t.Fatalf("expected second helper pod reconcile to succeed, got error: %v", err)
	}
	if created {
		t.Fatalf("expected second helper pod reconcile not to create pod")
	}

	if firstPod.GetName() != secondPod.GetName() {
		t.Fatalf("expected helper pod name to be stable across reconciles, got %s and %s", firstPod.GetName(), secondPod.GetName())
	}

	podList := &corev1.PodList{}
	if err := k8sClient.List(context.Background(), podList, client.InNamespace(ociHelperPodNamespace(promiseRelease))); err != nil {
		t.Fatalf("failed listing helper pods: %v", err)
	}
	if len(podList.Items) != 1 {
		t.Fatalf("expected 1 helper pod, got %d", len(podList.Items))
	}
}

func newPromiseReleaseUnitTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add corev1 scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("failed to add v1alpha1 scheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(sch).
		WithStatusSubresource(&v1alpha1.PromiseRelease{})
	if len(objects) > 0 {
		builder.WithObjects(objects...)
	}

	return builder.Build()
}
