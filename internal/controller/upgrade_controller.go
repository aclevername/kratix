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
	"context"
	"encoding/json"
	"fmt"
	"slices"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/resourceutil"

	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type UpgradeController struct {
	//use same naming conventions as other controllers
	Client                      client.Client
	GVK                         *schema.GroupVersionKind
	Scheme                      *runtime.Scheme
	PromiseIdentifier           string
	Log                         logr.Logger
	UID                         string
	Enabled                     *bool
	CRD                         *apiextensionsv1.CustomResourceDefinition
	PromiseDestinationSelectors []v1alpha1.PromiseScheduling
	CanCreateResources          *bool
	NumberOfJobsToKeep          int
	ReconciliationInterval      time.Duration
	EventRecorder               record.EventRecorder
}

var defaultUpgradeRequeue = ctrl.Result{Requeue: true, RequeueAfter: 1 * time.Second}

func (r *UpgradeController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	resourceRequestIdentifier := fmt.Sprintf("%s-%s", r.PromiseIdentifier, req.Name)
	logger := r.Log.WithValues(
		"uid", r.UID,
		"promiseID", r.PromiseIdentifier,
		"namespace", req.NamespacedName,
		"resourceRequest", resourceRequestIdentifier,
	)

	logger.Info("Reconciling UpgradeController")

	rr := &unstructured.Unstructured{}
	rr.SetGroupVersionKind(*r.GVK)

	promise := &v1alpha1.Promise{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: r.PromiseIdentifier}, promise); err != nil {
		logger.Error(err, "Failed getting Promise")
		return defaultRequeue, nil
	}

	logger.Info("Fetched Promise", "gen", promise.Generation, "promise", promise)

	if err := r.Client.Get(ctx, req.NamespacedName, rr); err != nil {
		if apierrors.IsNotFound(err) {
			return defaultRequeue, nil
		}
		logger.Error(err, "Failed getting Promise CRD")
		return defaultRequeue, nil
	}

	gvkList := r.GVK.GroupVersion().WithKind(r.GVK.Kind + "List")
	rrs := &unstructured.UnstructuredList{}
	rrs.SetGroupVersionKind(gvkList)
	if err := r.Client.List(ctx, rrs); err != nil {
		logger.Error(err, "Failed listing ResourceRequests")
		return defaultRequeue, nil
	}

	promiseGen := promise.Status.ObservedGeneration
	configMap := &corev1.ConfigMap{}
	configMapName := fmt.Sprintf("%s-%d-upgrade", r.PromiseIdentifier, promiseGen)
	logger.Info("Checking for upgrade plan configmap", "configMapName", configMapName)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: "default"}, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Upgrade plan configmap not found, creating")
			return defaultRequeue, r.createUpgradePlanConfigMap(ctx, configMapName, rrs, promise)
		}
		logger.Error(err, "Failed getting upgrade plan configmap")
		return defaultUpgradeRequeue, nil
	}

	logger.Info("Found upgrade plan configmap, checking if this RR is next in line", "configMap", configMap)

	upgradeOrder := configMap.Data["liveResourceRequests"]
	rrList := [][]string{}
	if err := json.Unmarshal([]byte(upgradeOrder), &rrList); err != nil {
		logger.Error(err, "Failed unmarshalling upgrade plan from configmap")
		return defaultUpgradeRequeue, nil
	}

	if len(rrList) == 0 {
		logger.Info("No ResourceRequests in upgrade plan, nothing to do")
		return defaultUpgradeRequeue, nil
	}

	if !slices.Contains(rrList[0], fmt.Sprintf("%s/%s", rr.GetNamespace(), rr.GetName())) {
		logger.Info("Not the latest ResourceRequest in the upgrade plan, requeuing", "rrList", rrList)
		return defaultUpgradeRequeue, nil
	}

	logger.Info("latest ResourceRequest in the upgrade plan, proceeding with upgrade", "rrList", rrList)

	upgradeLabel := "kratix.io/upgrade"
	value := rr.GetLabels()[upgradeLabel]
	upgradeID := fmt.Sprintf("%s-%d-upgrade", r.PromiseIdentifier, promiseGen)
	if value != upgradeID {
		logger.Info("triggering upgrade for this RR")
		labels := rr.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}

		labels[upgradeLabel] = upgradeID
		labels[resourceutil.ManualReconciliationLabel] = "true"
		rr.SetLabels(labels)
		logger.Info("Updating labels on ResourceRequest to trigger upgrade", "labels", labels)
		if err := r.Client.Update(ctx, rr); err != nil {
			logger.Error(err, "Failed updating ResourceRequest with upgrade label")
			return defaultUpgradeRequeue, nil
		}
		logger.Info("Successfully updated ResourceRequest with upgrade label, requeuing")

		return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, nil
	}

	logger.Info("RR has the upgrade label, checking if its finished upgrading")
	_, ok := rr.GetLabels()[resourceutil.ManualReconciliationLabel]
	if ok {
		logger.Info("RR is currently being manually reconciled, requeuing")
		return defaultUpgradeRequeue, nil
	}

	logger.Info("RR is no longer being manually reconciled, checking conditions are healthy")
	workflowedFinished := false
	for _, condition := range rr.Object["status"].(map[string]interface{})["conditions"].([]interface{}) {
		if "ConfigureWorkflowCompleted" == condition.(map[string]interface{})["type"] &&
			"True" == condition.(map[string]interface{})["status"] {
			workflowedFinished = true
			break
		}
	}

	if !workflowedFinished {
		logger.Info("RR has not finished upgrading, requeuing")
		return defaultUpgradeRequeue, nil
	}

	logger.Info("RR has finished upgrading, removing from upgrade plan")
	rrList, removed, last := removeRR(rrList, rr.GetNamespace(), rr.GetName())
	if !removed {
		logger.Info("RR was not found in upgrade plan, this should not happen")
	}
	if last {
		logger.Info("This was the last RR in the upgrade plan")
	}

	if len(rrList) == 0 {
		logger.Info("No more RRs to upgrade, setting configmap as empty")
	}

	marshalledRRList, err := json.Marshal(rrList)
	if err != nil {
		logger.Error(err, "Failed marshalling upgrade plan")
		return defaultUpgradeRequeue, nil
	}
	configMap.Data["liveResourceRequests"] = string(marshalledRRList)

	if err := r.Client.Update(ctx, configMap); err != nil {
		logger.Error(err, "Failed updating upgrade plan configmap")
		return defaultUpgradeRequeue, nil
	}

	logger.Info("Finished upgrade for this RR")
	return defaultUpgradeRequeue, nil
}

func (r *UpgradeController) createUpgradePlanConfigMap(ctx context.Context, name string, rrs *unstructured.UnstructuredList, promise *v1alpha1.Promise) error {
	configMap := &corev1.ConfigMap{}
	configMap.Name = name
	configMap.Namespace = "default"

	marshalledRRList, err := json.Marshal(promise.CompileUpgradeStrategy(rrs, nil))
	if err != nil {
		return err
	}
	// convert list to json and store in configmap data
	configMap.Data = map[string]string{
		"liveResourceRequests":     string(marshalledRRList),
		"originalResourceRequests": string(marshalledRRList),
	}

	r.Log.Info("Creating upgrade plan configmap", "configMap", configMap)

	return r.Client.Create(ctx, configMap)
}

func removeRR(rrList [][]string, ns, name string) ([][]string, bool, bool) {
	key := ns + "/" + name
	for i := 0; i < len(rrList); i++ {
		// find index in this wave
		idx := -1
		for k, it := range rrList[i] {
			if it == key {
				idx = k
				break
			}
		}
		if idx == -1 {
			continue
		}

		// remove from wave
		wave := append(rrList[i][:idx], rrList[i][idx+1:]...)
		if len(wave) == 0 {
			// drop empty wave
			rrList = append(rrList[:i], rrList[i+1:]...)
		} else {
			rrList[i] = wave
		}
		return rrList, true, len(rrList) == 0
	}
	return rrList, false, len(rrList) == 0
}
