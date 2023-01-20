package controllers

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"k8s.io/client-go/kubernetes/scheme"

	"github.com/go-logr/logr"
	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type Scheduler struct {
	Client client.Client
	Log    logr.Logger
}

func (r *Scheduler) ReconcileCluster() error {
	works := platformv1alpha1.WorkList{}
	lo := &client.ListOptions{
		Namespace: "default",
	}
	if err := r.Client.List(context.Background(), &works, lo); err != nil {
		return err
	}

	for _, work := range works.Items {
		if work.IsWorkerResource() {
			if err := r.ReconcileWork(&work); err != nil {
				r.Log.Error(err, "Failed reconciling Work: ")
			}
		}
	}

	return nil
}

func (r *Scheduler) ReconcileWork(work *platformv1alpha1.Work) error {
	targetClusterNames := r.getTargetClusterNames(work)
	if len(targetClusterNames) == 0 {
		r.Log.Info("no Clusters can be selected for clusterSelector", "clusterSelectors", labels.FormatLabels(work.Spec.ClusterSelector))
		return fmt.Errorf("no Clusters can be selected for clusterSelector")
	}

	r.Log.Info("found available target clusters", "clusters", targetClusterNames)
	return r.createWorkplacementsForTargetClusters(work, targetClusterNames)
}

func (r *Scheduler) createWorkplacementsForTargetClusters(work *platformv1alpha1.Work, targetClusterNames []string) error {
	for _, targetClusterName := range targetClusterNames {
		workPlacement := platformv1alpha1.WorkPlacement{}
		workPlacement.Namespace = "default"
		workPlacement.Name = work.Name + "." + targetClusterName
		workPlacement.Spec.WorkName = work.Name
		workPlacement.Spec.TargetClusterName = targetClusterName
		workPlacement.Labels = work.Labels
		controllerutil.AddFinalizer(&workPlacement, repoCleanupWorkPlacementFinalizer)

		if err := controllerutil.SetControllerReference(work, &workPlacement, scheme.Scheme); err != nil {
			r.Log.Error(err, "Error setting ownership")
			return err
		}

		if err := r.Client.Create(context.Background(), &workPlacement); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}

			r.Log.Error(err, "Error creating new WorkPlacement: "+workPlacement.Name)
			return err
		}
	}
	return nil
}

// Where Work is a Resource Request return one random Cluster name, where Work is a
// ClusterWorkerResource return all Cluster names
func (r *Scheduler) getTargetClusterNames(work *platformv1alpha1.Work) []string {
	if work.IsResourceRequest() {
		if len(work.Spec.WorkAffinity) > 0 {
			r.Log.Info("Checking if related Works have been scheduled to a cluster already", "work", work.GetName())

			if desiredCluster := r.checkIfClusterAlreadySelectedBySiblingWork(work); desiredCluster != "" {
				r.Log.Info("Releated Work is already scheduled, co-locating Work to the same cluster", "cluster", desiredCluster, "work", work.GetName())
				return []string{desiredCluster}
			}
		}
	}
	if work.Spec.ClusterSelector == nil {
		work.Spec.ClusterSelector = map[string]string{}
	}

	if len(work.Spec.WorkAffinity) > 0 {
		for _, siblingWorkLabel := range work.Spec.WorkAffinity {
			siblingWork := r.GetWorkWithLabel(siblingWorkLabel)
			if siblingWork == nil {
				panic("checked in controller, shouldn't happen")
			}
			work.Spec.ClusterSelector = labels.Merge(work.Spec.ClusterSelector, siblingWork.Spec.ClusterSelector)
		}
	}

	workerClusters := r.getWorkerClustersForWork(work)

	if len(workerClusters) == 0 {
		return make([]string, 0)
	}

	if work.IsResourceRequest() {
		r.Log.Info("Getting Worker cluster names for Resource Request")
		var targetClusterNames = make([]string, 1)
		rand.Seed(time.Now().UnixNano())
		randomClusterIndex := rand.Intn(len(workerClusters))
		targetClusterNames[0] = workerClusters[randomClusterIndex].Name
		r.Log.Info("Scheduling to cluster: " + targetClusterNames[0])
		return targetClusterNames
	} else if work.IsWorkerResource() {
		r.Log.Info("Getting Worker cluster names for Worker Resources")
		var targetClusterNames = make([]string, len(workerClusters))
		for i := 0; i < len(workerClusters); i++ {
			targetClusterNames[i] = workerClusters[i].Name
			r.Log.Info("Adding Worker Cluster: " + targetClusterNames[i])
		}
		return targetClusterNames
	} else {
		replicas := work.Spec.Replicas
		r.Log.Info("Cannot interpret replica count: " + fmt.Sprint(replicas))
		return make([]string, 0)
	}
}

// By default, all worker clusters are returned. However, if there are selectors provided, only matching clusters will be returned.
func (r *Scheduler) getWorkerClustersForWork(work *platformv1alpha1.Work) []platformv1alpha1.Cluster {
	workerClusters := &platformv1alpha1.ClusterList{}
	lo := &client.ListOptions{
		Namespace: "default",
	}

	if work.HasClusterSelector() {
		workSelectorLabel := labels.FormatLabels(work.Spec.ClusterSelector)
		selector, err := labels.Parse(workSelectorLabel)

		if err != nil {
			r.Log.Error(err, "error parsing cluster selector labels")
		}
		lo.LabelSelector = selector
	}

	r.Log.Info("listing clusters with selectors", "clusterSelectors", work.Spec.ClusterSelector)
	err := r.Client.List(context.Background(), workerClusters, lo)
	if err != nil {
		r.Log.Error(err, "Error listing available clusters")
	}

	return workerClusters.Items
}

func (r *Scheduler) checkIfClusterAlreadySelectedBySiblingWork(work *platformv1alpha1.Work) string {
	for _, workAffinity := range work.Spec.WorkAffinity {
		if workPlacement := r.GetWorkPlacementWithLabel(workAffinity); workPlacement != nil {
			return workPlacement.Spec.TargetClusterName
		}
	}

	return ""
}

func (r *Scheduler) GetWorkWithLabel(label string) *platformv1alpha1.Work {
	// r.Log.Info("getting work with label", "label", label)
	works := &platformv1alpha1.WorkList{}
	lo := &client.ListOptions{
		Namespace: "default",
	}

	workSelectorLabel := labels.FormatLabels(map[string]string{label: ""})
	selector, err := labels.Parse(workSelectorLabel)

	if err != nil {
		r.Log.Error(err, "error parsing cluster selector labels")
	}
	lo.LabelSelector = selector

	err = r.Client.List(context.Background(), works, lo)
	if err != nil {
		r.Log.Error(err, "Error listing available clusters")
	}
	if len(works.Items) > 0 {
		// r.Log.Info("work with label found", "label", label)
		return &works.Items[0]
	}

	// r.Log.Info("work with label not found", "label", label)
	return nil
}

func (r *Scheduler) GetWorkPlacementWithLabel(label string) *platformv1alpha1.WorkPlacement {
	// r.Log.Info("getting workPlacement with label", "label", label)
	workPlacements := &platformv1alpha1.WorkPlacementList{}
	lo := &client.ListOptions{
		Namespace: "default",
	}

	workSelectorLabel := labels.FormatLabels(map[string]string{label: ""})
	selector, err := labels.Parse(workSelectorLabel)

	if err != nil {
		r.Log.Error(err, "error parsing cluster selector labels")
	}
	lo.LabelSelector = selector

	err = r.Client.List(context.Background(), workPlacements, lo)
	if err != nil {
		r.Log.Error(err, "Error listing available clusters")
	}
	if len(workPlacements.Items) > 0 {
		// r.Log.Info("workPlacement with label found", "label", label)
		return &workPlacements.Items[0]
	}

	// r.Log.Info("workPlacement with label not found", "label", label)
	return nil
}
