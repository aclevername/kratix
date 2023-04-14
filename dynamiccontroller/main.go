package main

import (
	"flag"
	"io/ioutil"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	platformv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers"

	//+kubebuilder:scaffold:imports
	"context"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/controllers"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/uuid"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	conditionsutil "sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type Config struct {
	Selectors  string
	Identifier string
	Pipeline   string
	UID        string
	Group      string
	Version    string
	Kind       string
}

var setupLog = ctrl.Log.WithName("setup")

func init() {
	utilruntime.Must(platformv1alpha1.AddToScheme(scheme.Scheme))
	//+kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var repositoryType string
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.StringVar(&repositoryType, "repository-type", writers.S3, "The type of the repository Kratix will communicate with")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts), func(o *zap.Options) {
		o.TimeEncoder = zapcore.TimeEncoderOfLayout("2006-01-02T15:04:05Z07:00")
	}))

	config := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme.Scheme,
		MetricsBindAddress:     metricsAddr,
		Port:                   9443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2743c979.kratix.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	dynamicConfig := Config{
		Selectors:  readOrExit("/config/selectors"),
		Identifier: readOrExit("/config/identifier"),
		Pipeline:   readOrExit("/config/pipeline"),
		UID:        readOrExit("/config/uid"),
		Group:      readOrExit("/config/group"),
		Version:    readOrExit("/config/version"),
		Kind:       readOrExit("/config/kind"),
	}
	var selectors labels.Set

	if dynamicConfig.Selectors != "<none>" {
		selectors, err = labels.ConvertSelectorToLabelsMap(dynamicConfig.Selectors)

		if err != nil {
			setupLog.Error(err, "unable to gen selectors")
			os.Exit(1)
		}
	}

	rrGVK := schema.GroupVersionKind{
		Group:   dynamicConfig.Group,
		Version: dynamicConfig.Version,
		Kind:    dynamicConfig.Kind,
	}

	dynamicResourceRequestController := DynamicResourceRequestController{
		Client:                 mgr.GetClient(),
		scheme:                 mgr.GetScheme(),
		gvk:                    &rrGVK,
		promiseIdentifier:      dynamicConfig.Identifier,
		promiseClusterSelector: selectors,
		xaasRequestPipeline:    strings.Split(dynamicConfig.Pipeline, ","),
		uid:                    dynamicConfig.UID,
		log:                    ctrl.Log.WithName("controllers").WithName("Dynamic").WithName(dynamicConfig.Identifier),
	}

	unstructuredCRD := &unstructured.Unstructured{}
	unstructuredCRD.SetGroupVersionKind(rrGVK)

	ctrl.NewControllerManagedBy(mgr).
		For(unstructuredCRD).
		Complete(&dynamicResourceRequestController)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func readOrExit(filename string) string {
	bytes, err := ioutil.ReadFile(filename)
	if err != nil {
		setupLog.Error(err, "unable to read config")
		os.Exit(1)
	}
	return string(bytes)
}

const (
	workFinalizer     = controllers.FinalizerPrefix + "work-cleanup"
	pipelineFinalizer = controllers.FinalizerPrefix + "pipeline-cleanup"
)

var (
	rrFinalizers = []string{workFinalizer, pipelineFinalizer}
	// fastRequeue can be used whenever we want to quickly requeue, and we don't expect
	// an error to occur. Example: we delete a resource, we then requeue
	// to check it's been deleted. Here we can use a fastRequeue instead of a defaultRequeue
	fastRequeue    = ctrl.Result{RequeueAfter: 1 * time.Second}
	defaultRequeue = ctrl.Result{RequeueAfter: 5 * time.Second}
	slowRequeue    = ctrl.Result{RequeueAfter: 15 * time.Second}
)

type DynamicResourceRequestController struct {
	//use same naming conventions as other controllers
	Client                 client.Client
	gvk                    *schema.GroupVersionKind
	scheme                 *runtime.Scheme
	promiseIdentifier      string
	promiseClusterSelector labels.Set
	xaasRequestPipeline    []string
	log                    logr.Logger
	finalizers             []string
	uid                    string
}

func (r *DynamicResourceRequestController) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.log.WithValues("uid", r.uid, r.promiseIdentifier, req.NamespacedName)
	resourceRequestIdentifier := fmt.Sprintf("%s-%s-%s", r.promiseIdentifier, req.Namespace, req.Name)

	rr := &unstructured.Unstructured{}
	rr.SetGroupVersionKind(*r.gvk)

	err := r.Client.Get(ctx, req.NamespacedName, rr)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed getting Promise CRD")
		return defaultRequeue, nil
	}

	if !rr.GetDeletionTimestamp().IsZero() {
		return r.deleteResources(ctx, rr, resourceRequestIdentifier, logger)
	}

	// Reconcile necessary finalizers
	if controllers.FinalizersAreMissing(rr, []string{workFinalizer, pipelineFinalizer}) {
		return controllers.AddFinalizers(ctx, r.Client, rr, []string{workFinalizer, pipelineFinalizer}, logger)
	}

	if r.pipelineHasExecuted(resourceRequestIdentifier) {
		logger.Info("Cannot execute update on pre-existing pipeline for Promise resource request " + resourceRequestIdentifier)
		return ctrl.Result{}, nil
	}

	err = r.setPipelineCondition(ctx, rr, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	volumes := []v1.Volume{
		{Name: "metadata", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
		{
			Name: "promise-cluster-selectors",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: "cluster-selectors-" + r.promiseIdentifier,
					},
					Items: []v1.KeyToPath{{
						Key:  "selectors",
						Path: "promise-cluster-selectors",
					}},
				},
			},
		},
	}
	initContainers, pipelineVolumes := r.pipelineInitContainers(resourceRequestIdentifier, req)
	volumes = append(volumes, pipelineVolumes...)

	rrKind := fmt.Sprintf("%s.%s", strings.ToLower(r.gvk.Kind), r.gvk.Group)
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "request-pipeline-" + r.promiseIdentifier + "-" + getShortUuid(),
			Namespace: "default",
			Labels: map[string]string{
				"kratix-promise-id":                  r.promiseIdentifier,
				"kratix-promise-resource-request-id": resourceRequestIdentifier,
			},
		},
		Spec: v1.PodSpec{
			RestartPolicy:      v1.RestartPolicyOnFailure,
			ServiceAccountName: r.promiseIdentifier + "-promise-pipeline",
			Containers: []v1.Container{
				{
					Name:    "status-writer",
					Image:   os.Getenv("WC_IMG"),
					Command: []string{"sh", "-c", "update-status"},
					Env: []v1.EnvVar{
						{Name: "RR_KIND", Value: rrKind},
						{Name: "RR_NAME", Value: req.Name},
						{Name: "RR_NAMESPACE", Value: req.Namespace},
					},
					VolumeMounts: []v1.VolumeMount{{
						MountPath: "/work-creator-files/metadata",
						Name:      "metadata",
					}},
				},
			},
			InitContainers: initContainers,
			Volumes:        volumes,
		},
	}

	logger.Info("Creating Pipeline for Promise resource request: " + resourceRequestIdentifier + ". The pipeline will now execute...")
	err = r.Client.Create(ctx, &pod)
	if err != nil {
		logger.Error(err, "Error creating Pod")
		y, _ := yaml.Marshal(&pod)
		logger.Error(err, string(y))
	}

	return ctrl.Result{}, nil
}

func (r *DynamicResourceRequestController) pipelineHasExecuted(resourceRequestIdentifier string) bool {
	isPromise, _ := labels.NewRequirement("kratix-promise-resource-request-id", selection.Equals, []string{resourceRequestIdentifier})
	selector := labels.NewSelector().
		Add(*isPromise)

	listOps := &client.ListOptions{
		Namespace:     "default",
		LabelSelector: selector,
	}

	ol := &v1.PodList{}
	err := r.Client.List(context.Background(), ol, listOps)
	if err != nil {
		fmt.Println(err.Error())
		return false
	}
	return len(ol.Items) > 0
}

func (r *DynamicResourceRequestController) deleteResources(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier string, logger logr.Logger) (ctrl.Result, error) {
	if controllers.FinalizersAreDeleted(resourceRequest, rrFinalizers) {
		return ctrl.Result{}, nil
	}

	if controllerutil.ContainsFinalizer(resourceRequest, workFinalizer) {
		err := r.deleteWork(ctx, resourceRequest, resourceRequestIdentifier, workFinalizer, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	if controllerutil.ContainsFinalizer(resourceRequest, pipelineFinalizer) {
		err := r.deletePipeline(ctx, resourceRequest, resourceRequestIdentifier, pipelineFinalizer, logger)
		if err != nil {
			return defaultRequeue, err
		}
		return fastRequeue, nil
	}

	return fastRequeue, nil
}

func (r *DynamicResourceRequestController) deleteWork(ctx context.Context, resourceRequest *unstructured.Unstructured, workName string, finalizer string, logger logr.Logger) error {
	work := &v1alpha1.Work{}
	err := r.Client.Get(ctx, types.NamespacedName{
		Namespace: "default",
		Name:      workName,
	}, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.Client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error locating Work, will try again in 5 seconds", "workName", workName)
		return err
	}

	err = r.Client.Delete(ctx, work)
	if err != nil {
		if errors.IsNotFound(err) {
			// only remove finalizer at this point because deletion success is guaranteed
			controllerutil.RemoveFinalizer(resourceRequest, finalizer)
			if err := r.Client.Update(ctx, resourceRequest); err != nil {
				return err
			}
			return nil
		}

		logger.Error(err, "Error deleting Work %s, will try again in 5 seconds", "workName", workName)
		return err
	}

	return nil
}

func (r *DynamicResourceRequestController) deletePipeline(ctx context.Context, resourceRequest *unstructured.Unstructured, resourceRequestIdentifier, finalizer string, logger logr.Logger) error {
	podGVK := schema.GroupVersionKind{
		Group:   v1.SchemeGroupVersion.Group,
		Version: v1.SchemeGroupVersion.Version,
		Kind:    "Pod",
	}

	podLabels := map[string]string{
		"kratix-promise-id":                  r.promiseIdentifier,
		"kratix-promise-resource-request-id": resourceRequestIdentifier,
	}

	resourcesRemaining, err := controllers.DeleteAllResourcesWithKindMatchingLabel(ctx, r.Client, podGVK, podLabels, "default", logger)
	if err != nil {
		return err
	}

	if !resourcesRemaining {
		controllerutil.RemoveFinalizer(resourceRequest, finalizer)
		if err := r.Client.Update(ctx, resourceRequest); err != nil {
			return err
		}
	}

	return nil
}

func getShortUuid() string {
	envUuid, present := os.LookupEnv("TEST_PROMISE_CONTROLLER_POD_IDENTIFIER_UUID")
	if present {
		return envUuid
	} else {
		return string(uuid.NewUUID()[0:5])
	}
}

func (r *DynamicResourceRequestController) setPipelineCondition(ctx context.Context, rr *unstructured.Unstructured, logger logr.Logger) error {
	setter := conditionsutil.UnstructuredSetter(rr)
	getter := conditionsutil.UnstructuredGetter(rr)
	condition := conditionsutil.Get(getter, clusterv1.ConditionType("PipelineCompleted"))
	if condition == nil {
		err := unstructured.SetNestedMap(rr.Object, map[string]interface{}{"message": "Pending"}, "status")
		if err != nil {
			logger.Error(err, "failed to set status.message to pending, ignoring error")
		}
		conditionsutil.Set(setter, &clusterv1.Condition{
			Type:               clusterv1.ConditionType("PipelineCompleted"),
			Status:             v1.ConditionFalse,
			Message:            "Pipeline has not completed",
			Reason:             "PipelineNotCompleted",
			LastTransitionTime: metav1.NewTime(time.Now()),
		})
		logger.Info("setting condition PipelineCompleted false")
		if err := r.Client.Status().Update(ctx, rr); err != nil {
			return err
		}
	}
	return nil
}

func (r *DynamicResourceRequestController) pipelineInitContainers(rrID string, req ctrl.Request) ([]v1.Container, []v1.Volume) {
	resourceKindNameNamespace := fmt.Sprintf("%s.%s %s --namespace %s", strings.ToLower(r.gvk.Kind), r.gvk.Group, req.Name, req.Namespace)
	metadataVolumeMount := v1.VolumeMount{MountPath: "/metadata", Name: "metadata"}

	resourceRequestCommand := fmt.Sprintf("kubectl get %s -oyaml > /output/object.yaml", resourceKindNameNamespace)
	reader := v1.Container{
		Name:    "reader",
		Image:   "bitnami/kubectl:1.20.10",
		Command: []string{"sh", "-c", resourceRequestCommand},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/output",
				Name:      "vol0",
			},
		},
	}

	volumes := []v1.Volume{{Name: "vol0", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}}

	containers := []v1.Container{reader}
	for i, c := range r.xaasRequestPipeline {
		volumes = append(volumes, v1.Volume{
			Name:         "vol" + strconv.Itoa(i+1),
			VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
		})
		containers = append(containers, v1.Container{
			Name:  fmt.Sprintf("xaas-request-pipeline-stage-%d", i),
			Image: c,
			VolumeMounts: []v1.VolumeMount{
				metadataVolumeMount,
				{Name: "vol" + strconv.Itoa(i), MountPath: "/input"},
				{Name: "vol" + strconv.Itoa(i+1), MountPath: "/output"},
			},
		})
	}

	workCreatorCommand := fmt.Sprintf("./work-creator -identifier %s -input-directory /work-creator-files", rrID)
	writer := v1.Container{
		Name:    "work-writer",
		Image:   os.Getenv("WC_IMG"),
		Command: []string{"sh", "-c", workCreatorCommand},
		VolumeMounts: []v1.VolumeMount{
			{
				MountPath: "/work-creator-files/input",
				Name:      "vol" + strconv.Itoa(len(containers)-1),
			},
			{
				MountPath: "/work-creator-files/metadata",
				Name:      "metadata",
			},
			{
				MountPath: "/work-creator-files/kratix-system",
				Name:      "promise-cluster-selectors",
			},
		},
	}

	containers = append(containers, writer)

	return containers, volumes
}
