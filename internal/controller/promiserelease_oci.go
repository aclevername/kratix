/*
Copyright 2021 Syntasso.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/internal/logging"
)

const (
	ociHelperContainerName = "promise"

	conditionReasonCreatingPod     = "CreatingPod"
	conditionReasonWaitingForPod   = "WaitingForPod"
	conditionReasonExecFailed      = "ExecFailed"
	conditionReasonManifestMissing = "ManifestNotFound"
	conditionReasonParseFailed     = "ParseFailed"
	conditionReasonApplyFailed     = "ApplyFailed"
	conditionReasonWaitingForCRD   = "WaitingForCRD"

	ociPodWaitRequeueDelay = 2 * time.Second
	ociCRDRequeueDelay     = 5 * time.Second
)

// ExecRunner executes commands via the Pod exec subresource.
type ExecRunner interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout string, stderr string, err error)
}

type SPDYExecRunner struct {
	config *rest.Config
	client kubernetes.Interface
}

func NewSPDYExecRunner(config *rest.Config) (*SPDYExecRunner, error) {
	if config == nil {
		return nil, fmt.Errorf("rest config must not be nil")
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed creating kubernetes clientset for exec: %w", err)
	}

	return &SPDYExecRunner{
		config: config,
		client: clientset,
	}, nil
}

func (r *SPDYExecRunner) Exec(ctx context.Context, namespace, podName, container string, command []string) (string, string, error) {
	req := r.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to initialise exec executor: %w", err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), stderr.String(), err
	}

	return stdout.String(), stderr.String(), nil
}

func (r *PromiseReleaseReconciler) reconcileOCISource(o opts, promiseRelease *v1alpha1.PromiseRelease) (ctrl.Result, error) {
	if r.ExecRunner == nil {
		return ctrl.Result{}, fmt.Errorf("exec runner is not configured for OCI PromiseRelease source")
	}

	helperPod, created, err := r.reconcileOCIHelperPod(o, promiseRelease)
	if err != nil {
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse,
			"Failed to create or reconcile helper Pod", conditionReasonCreatingPod, nil)
		return ctrl.Result{}, fmt.Errorf("failed to reconcile helper pod: %w", err)
	}

	helperPodName := helperPod.GetName()
	statusUpdate := &promiseReleaseStatusUpdate{
		helperPodName: &helperPodName,
	}

	resolvedImageID := getResolvedImageID(helperPod)
	if resolvedImageID != "" {
		statusUpdate.resolvedImageID = &resolvedImageID
	}

	switch helperPod.Status.Phase {
	case corev1.PodPending:
		reason := conditionReasonWaitingForPod
		message := fmt.Sprintf("Waiting for helper Pod %q to be running", helperPod.GetName())
		if created {
			reason = conditionReasonCreatingPod
			message = fmt.Sprintf("Created helper Pod %q, waiting for it to be running", helperPod.GetName())
		}
		r.updateStatusAndConditions(o, promiseRelease, statusInstalling, metav1.ConditionUnknown, message, reason, statusUpdate)
		return ctrl.Result{RequeueAfter: ociPodWaitRequeueDelay}, nil
	case corev1.PodFailed:
		msg := fmt.Sprintf("Helper Pod %q failed: %s", helperPod.GetName(), helperPodFailureMessage(helperPod))
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse, msg, conditionReasonExecFailed, statusUpdate)
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, fmt.Errorf("helper pod failed before manifest read: %s", helperPodFailureMessage(helperPod))
	case corev1.PodSucceeded:
		msg := fmt.Sprintf("Helper Pod %q completed before manifest could be read", helperPod.GetName())
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse, msg, conditionReasonExecFailed, statusUpdate)
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, fmt.Errorf("helper pod succeeded before manifest read")
	case corev1.PodRunning:
		// continue
	default:
		r.updateStatusAndConditions(o, promiseRelease, statusInstalling, metav1.ConditionUnknown,
			fmt.Sprintf("Helper Pod %q is in phase %q", helperPod.GetName(), helperPod.Status.Phase),
			conditionReasonWaitingForPod, statusUpdate)
		return ctrl.Result{RequeueAfter: ociPodWaitRequeueDelay}, nil
	}

	manifestPath := ociManifestPath(promiseRelease.Spec.SourceRef)
	stdout, stderr, err := r.ExecRunner.Exec(o.ctx, helperPod.GetNamespace(), helperPod.GetName(), ociHelperContainerName, []string{"cat", manifestPath})
	if err != nil {
		if isManifestNotFoundError(err, stderr) {
			msg := fmt.Sprintf("Manifest path %q not found in image %q", manifestPath, promiseRelease.Spec.SourceRef.Image)
			r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse, msg, conditionReasonManifestMissing, statusUpdate)
			r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
			return ctrl.Result{}, fmt.Errorf("manifest %q not found in helper image: %w", manifestPath, err)
		}

		msg := fmt.Sprintf("Failed to exec helper Pod %q: %s", helperPod.GetName(), summarizeExecError(stderr, err))
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse, msg, conditionReasonExecFailed, statusUpdate)
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, fmt.Errorf("failed reading manifest from helper pod: %w", err)
	}

	manifestObjects, err := decodeManifestDocuments([]byte(stdout))
	if err != nil {
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse,
			fmt.Sprintf("Failed to parse manifest bytes from %q", manifestPath), conditionReasonParseFailed, statusUpdate)
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, fmt.Errorf("failed to parse OCI manifest: %w", err)
	}

	promise, err := findPromiseFromManifestObjects(manifestObjects)
	if err != nil {
		r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse,
			"Failed to find Promise object in manifest", conditionReasonParseFailed, statusUpdate)
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, fmt.Errorf("failed to find promise in manifest: %w", err)
	}

	updated, err := r.validateVersion(o, promiseRelease, promise)
	if err != nil {
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, err
	}
	if updated {
		r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
		return ctrl.Result{}, nil
	}

	for i := range manifestObjects {
		obj := manifestObjects[i].DeepCopy()
		if obj.GetKind() == "Promise" {
			if err := r.installPromise(o, promiseRelease, promise); err != nil {
				r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse,
					"Failed to create or update Promise", conditionReasonApplyFailed, statusUpdate)
				r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
				return ctrl.Result{}, fmt.Errorf("failed to create or update promise: %w", err)
			}
			continue
		}

		if err := r.applyManifestResource(o, promiseRelease, obj); err != nil {
			if isNoMatchError(err) {
				msg := fmt.Sprintf("Waiting for CRD before applying %s %s", obj.GetKind(), obj.GetName())
				r.updateStatusAndConditions(o, promiseRelease, statusInstalling, metav1.ConditionUnknown, msg, conditionReasonWaitingForCRD, statusUpdate)
				return ctrl.Result{RequeueAfter: ociCRDRequeueDelay}, nil
			}

			msg := fmt.Sprintf("Failed to apply %s %s", obj.GetKind(), obj.GetName())
			r.updateStatusAndConditions(o, promiseRelease, statusErrorInstalling, metav1.ConditionFalse, msg, conditionReasonApplyFailed, statusUpdate)
			r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
			return ctrl.Result{}, fmt.Errorf("failed to apply manifest resource %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	now := metav1.Now()
	r.updateStatusAndConditions(o, promiseRelease, statusInstalled, metav1.ConditionTrue, conditionMessageInstalled, conditionReasonInstalled, &promiseReleaseStatusUpdate{
		helperPodName:   &helperPodName,
		resolvedImageID: statusUpdate.resolvedImageID,
		lastAppliedTime: &now,
	})
	r.cleanupOCIHelperPodBestEffort(o, promiseRelease, helperPod.GetName())
	return ctrl.Result{}, nil
}

func (r *PromiseReleaseReconciler) reconcileOCIHelperPod(o opts, promiseRelease *v1alpha1.PromiseRelease) (*corev1.Pod, bool, error) {
	podName := ociHelperPodName(promiseRelease)
	namespace := ociHelperPodNamespace(promiseRelease)
	key := types.NamespacedName{
		Name:      podName,
		Namespace: namespace,
	}

	existingPod := &corev1.Pod{}
	if err := o.client.Get(o.ctx, key, existingPod); err == nil {
		return existingPod, false, nil
	} else if !errors.IsNotFound(err) {
		return nil, false, err
	}

	timeoutSeconds := ociTimeoutSeconds(promiseRelease.Spec.SourceRef)
	helperPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				promiseReleaseNameLabel:               promiseRelease.GetName(),
				v1alpha1.KratixPrefix + "source-type": "oci",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &timeoutSeconds,
			ServiceAccountName:    promiseRelease.Spec.SourceRef.ServiceAccountName,
			ImagePullSecrets:      promiseRelease.Spec.SourceRef.ImagePullSecrets,
			Containers: []corev1.Container{
				{
					Name:  ociHelperContainerName,
					Image: promiseRelease.Spec.SourceRef.Image,
					// Keep the container running long enough for pods/exec; this assumes /bin/sh is present in the image.
					Command: []string{"sh", "-c", "sleep 3600"},
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(promiseRelease, helperPod, r.Scheme); err != nil {
		return nil, false, err
	}

	if err := o.client.Create(o.ctx, helperPod); err != nil {
		if !errors.IsAlreadyExists(err) {
			return nil, false, err
		}
		if getErr := o.client.Get(o.ctx, key, existingPod); getErr != nil {
			return nil, false, getErr
		}
		return existingPod, false, nil
	}

	return helperPod, true, nil
}

func (r *PromiseReleaseReconciler) cleanupOCIHelperPodBestEffort(o opts, promiseRelease *v1alpha1.PromiseRelease, podName string) {
	if podName == "" {
		return
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: ociHelperPodNamespace(promiseRelease),
		},
	}
	if err := o.client.Delete(o.ctx, pod); err != nil && !errors.IsNotFound(err) {
		logging.Warn(o.logger, "failed to delete OCI helper Pod",
			"name", pod.GetName(),
			"namespace", pod.GetNamespace(),
			"error", err,
		)
	}
}

func (r *PromiseReleaseReconciler) applyManifestResource(o opts, promiseRelease *v1alpha1.PromiseRelease, obj *unstructured.Unstructured) error {
	if err := ctrl.SetControllerReference(promiseRelease, obj, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference on %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}

	return o.client.Patch(o.ctx, obj, client.Apply, client.FieldOwner(promiseReleaseFieldManager), client.ForceOwnership)
}

func decodeManifestDocuments(manifestBytes []byte) ([]unstructured.Unstructured, error) {
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifestBytes), 4096)
	objects := make([]unstructured.Unstructured, 0)
	for {
		var obj map[string]interface{}
		err := decoder.Decode(&obj)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		if len(obj) == 0 {
			continue
		}

		u := unstructured.Unstructured{Object: obj}
		if u.GetKind() == "" && u.GetAPIVersion() == "" {
			continue
		}
		objects = append(objects, u)
	}

	if len(objects) == 0 {
		return nil, fmt.Errorf("manifest did not contain any Kubernetes resources")
	}

	return objects, nil
}

func findPromiseFromManifestObjects(manifestObjects []unstructured.Unstructured) (*v1alpha1.Promise, error) {
	var promiseObject *unstructured.Unstructured

	for i := range manifestObjects {
		if manifestObjects[i].GetKind() != "Promise" {
			continue
		}
		if promiseObject != nil {
			return nil, fmt.Errorf("expected exactly one Promise object in manifest, found multiple")
		}
		promiseObject = manifestObjects[i].DeepCopy()
	}

	if promiseObject == nil {
		return nil, fmt.Errorf("no Promise object found in manifest")
	}

	promise := &v1alpha1.Promise{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(promiseObject.Object, promise); err != nil {
		return nil, fmt.Errorf("failed to decode Promise object: %w", err)
	}

	return promise, nil
}

func ociHelperPodName(promiseRelease *v1alpha1.PromiseRelease) string {
	normalizedName := normalizeRFC1123Segment(promiseRelease.GetName())
	if normalizedName == "" {
		normalizedName = "promiserelease"
	}

	hashInput := fmt.Sprintf("%s|%s|%d", promiseRelease.Spec.SourceRef.Image, ociManifestPath(promiseRelease.Spec.SourceRef), promiseRelease.GetGeneration())
	sum := sha256.Sum256([]byte(hashInput))
	suffix := hex.EncodeToString(sum[:])[:10]

	maxNameLength := 63
	fixedPartLength := len("pr-oci--") + len(suffix)
	maxBaseLength := maxNameLength - fixedPartLength
	if maxBaseLength < 1 {
		maxBaseLength = 1
	}
	if len(normalizedName) > maxBaseLength {
		normalizedName = strings.Trim(normalizedName[:maxBaseLength], "-")
	}
	if normalizedName == "" {
		normalizedName = "pr"
	}

	return fmt.Sprintf("pr-oci-%s-%s", normalizedName, suffix)
}

func ociHelperPodNamespace(promiseRelease *v1alpha1.PromiseRelease) string {
	if promiseRelease.GetNamespace() != "" {
		return promiseRelease.GetNamespace()
	}

	return v1alpha1.SystemNamespace
}

func ociManifestPath(sourceRef v1alpha1.SourceRef) string {
	if sourceRef.ManifestPath == "" {
		return v1alpha1.DefaultOCIPromiseManifestPath
	}

	return sourceRef.ManifestPath
}

func ociTimeoutSeconds(sourceRef v1alpha1.SourceRef) int64 {
	if sourceRef.TimeoutSeconds == nil || *sourceRef.TimeoutSeconds <= 0 {
		return v1alpha1.DefaultOCITimeoutSeconds
	}

	return *sourceRef.TimeoutSeconds
}

func isManifestNotFoundError(execErr error, stderr string) bool {
	errMsg := strings.ToLower(stderr + " " + execErr.Error())
	return strings.Contains(errMsg, "no such file or directory")
}

func summarizeExecError(stderr string, err error) string {
	stderr = strings.TrimSpace(stderr)
	if stderr != "" {
		return stderr
	}
	return err.Error()
}

func isNoMatchError(err error) bool {
	if err == nil {
		return false
	}
	if apimeta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		return true
	}

	return strings.Contains(strings.ToLower(err.Error()), "no matches for kind")
}

func normalizeRFC1123Segment(value string) string {
	normalized := strings.ToLower(value)
	var b strings.Builder
	b.Grow(len(normalized))

	lastWasDash := false
	for _, ch := range normalized {
		isAlphaNum := (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')
		if isAlphaNum {
			b.WriteRune(ch)
			lastWasDash = false
			continue
		}

		if !lastWasDash {
			b.WriteRune('-')
			lastWasDash = true
		}
	}

	return strings.Trim(b.String(), "-")
}

func helperPodFailureMessage(pod *corev1.Pod) string {
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}

	for _, containerStatus := range pod.Status.ContainerStatuses {
		if containerStatus.State.Waiting != nil && containerStatus.State.Waiting.Message != "" {
			return containerStatus.State.Waiting.Message
		}
		if containerStatus.State.Terminated != nil && containerStatus.State.Terminated.Message != "" {
			return containerStatus.State.Terminated.Message
		}
	}

	return "unknown failure"
}

func getResolvedImageID(pod *corev1.Pod) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != ociHelperContainerName {
			continue
		}
		if status.ImageID != "" {
			return status.ImageID
		}
	}

	return ""
}
