package pipeline

import (
	"fmt"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/resourceutil"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	kratixActionEnvVar  = "KRATIX_WORKFLOW_ACTION"
	kratixTypeEnvVar    = "KRATIX_WORKFLOW_TYPE"
	kratixPromiseEnvVar = "KRATIX_PROMISE_NAME"
)

func defaultPipelineVolumes() ([]v1.Volume, []v1.VolumeMount) {
	volumes := []v1.Volume{
		{Name: "shared-input", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
		{Name: "shared-output", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
		{Name: "shared-metadata", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
	}
	volumeMounts := []v1.VolumeMount{
		{MountPath: "/kratix/input", Name: "shared-input", ReadOnly: true},
		{MountPath: "/kratix/output", Name: "shared-output"},
		{MountPath: "/kratix/metadata", Name: "shared-metadata"},
	}
	return volumes, volumeMounts
}

func serviceAccount(args PipelineArgs) *v1.ServiceAccount {
	return &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.ServiceAccountName(),
			Namespace: args.Namespace(),
			Labels:    args.Labels(),
		},
	}
}

func role(obj *unstructured.Unstructured, objPluralName string, args PipelineArgs) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.RoleBindingName(),
			Labels:    args.Labels(),
			Namespace: args.Namespace(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{obj.GroupVersionKind().Group},
				Resources: []string{objPluralName, objPluralName + "/status"},
				Verbs:     []string{"get", "list", "update", "create", "patch"},
			},
			{
				APIGroups: []string{"platform.kratix.io"},
				Resources: []string{"works"},
				Verbs:     []string{"*"},
			},
		},
	}
}

func roleBinding(args PipelineArgs) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      args.RoleBindingName(),
			Labels:    args.Labels(),
			Namespace: args.Namespace(),
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "Role",
			APIGroup: "rbac.authorization.k8s.io",
			Name:     args.RoleName(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Namespace: args.Namespace(),
				Name:      args.ServiceAccountName(),
			},
		},
	}
}

func clusterRole(args PipelineArgs) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:   args.RoleName(),
			Labels: args.Labels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"platform.kratix.io"},
				Resources: []string{v1alpha1.PromisePlural, v1alpha1.PromisePlural + "/status", "works"},
				Verbs:     []string{"get", "list", "update", "create", "patch"},
			},
		},
	}
}

func clusterRoleBinding(args PipelineArgs) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:   args.RoleBindingName(),
			Labels: args.Labels(),
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			APIGroup: "rbac.authorization.k8s.io",
			Name:     args.RoleName(),
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Namespace: args.Namespace(),
				Name:      args.ServiceAccountName(),
			},
		},
	}
}

func destinationSelectorsConfigMap(resources PipelineArgs, destinationSelectors []v1alpha1.PromiseScheduling, promiseWorkflowSelectors *v1alpha1.WorkloadGroupScheduling) (*v1.ConfigMap, error) {
	workloadGroupScheduling := []v1alpha1.WorkloadGroupScheduling{}
	for _, scheduling := range destinationSelectors {
		workloadGroupScheduling = append(workloadGroupScheduling, v1alpha1.WorkloadGroupScheduling{
			MatchLabels: scheduling.MatchLabels,
			Source:      "promise",
		})
	}

	if promiseWorkflowSelectors != nil {
		workloadGroupScheduling = append(workloadGroupScheduling, *promiseWorkflowSelectors)
	}

	schedulingYAML, err := yaml.Marshal(workloadGroupScheduling)
	if err != nil {
		return nil, errors.Wrap(err, "error marshalling destinationSelectors to yaml")
	}

	return &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resources.ConfigMapName(),
			Namespace: resources.Namespace(),
			Labels:    resources.Labels(),
		},
		Data: map[string]string{
			"destinationSelectors": string(schedulingYAML),
		},
	}, nil
}

func readerContainer(obj *unstructured.Unstructured, kratixWorkflowType v1alpha1.Type, volumeName string) v1.Container {
	namespace := obj.GetNamespace()
	if namespace == "" {
		// if namespace is empty it means its a unnamespaced resource, so providing
		// any value is valid for kubectl
		namespace = v1alpha1.SystemNamespace
	}

	readerContainer := v1.Container{
		Name:  "reader",
		Image: os.Getenv("WC_IMG"),
		Env: []v1.EnvVar{
			{Name: "OBJECT_KIND", Value: strings.ToLower(obj.GetKind())},
			{Name: "OBJECT_GROUP", Value: obj.GroupVersionKind().Group},
			{Name: "OBJECT_NAME", Value: obj.GetName()},
			{Name: "OBJECT_NAMESPACE", Value: namespace},
			{Name: "KRATIX_WORKFLOW_TYPE", Value: string(kratixWorkflowType)},
		},
		VolumeMounts: []v1.VolumeMount{
			{MountPath: "/kratix/input", Name: "shared-input"},
			{MountPath: "/kratix/output", Name: "shared-output"},
		},
		Command: []string{"sh", "-c", "reader"},
	}
	return readerContainer
}

func generateContainersAndVolumes(obj *unstructured.Unstructured, workflowType v1alpha1.Type, pipeline v1alpha1.Pipeline, kratixEnvVars []v1.EnvVar) ([]v1.Container, []v1.Volume) {
	volumes, defaultVolumeMounts := defaultPipelineVolumes()

	readerContainer := readerContainer(obj, workflowType, "shared-input")
	containers := []v1.Container{
		readerContainer,
	}

	if len(pipeline.Spec.Volumes) > 0 {
		volumes = append(volumes, pipeline.Spec.Volumes...)
	}

	for _, c := range pipeline.Spec.Containers {
		containerVolumeMounts := append(defaultVolumeMounts, c.VolumeMounts...)

		containers = append(containers, v1.Container{
			Name:            c.Name,
			Image:           c.Image,
			VolumeMounts:    containerVolumeMounts,
			Args:            c.Args,
			Command:         c.Command,
			Env:             append(kratixEnvVars, c.Env...),
			EnvFrom:         c.EnvFrom,
			ImagePullPolicy: c.ImagePullPolicy,
		})
	}

	return containers, volumes
}

func generateRBAC(logger logr.Logger, args PipelineArgs, pipeline v1alpha1.Pipeline) ([]*rbacv1.ClusterRole, []*rbacv1.Role, []*rbacv1.RoleBinding) {
	var clusterRoles []*rbacv1.ClusterRole
	var roles []*rbacv1.Role
	var roleBindings []*rbacv1.RoleBinding

	for i, rbac := range pipeline.Spec.RBAC {

		var roleBindingNamespace = args.Namespace()
		policyRule := rbac
		role := "Role"
		for _, resource := range policyRule.Resources {
			// if resource contains a slash, set ClusterRole to true
			if strings.Contains(resource, "/") {
				roleBindingNamespace = strings.Split(resource, "/")[0]
			}
		}

		if roleBindingNamespace != args.Namespace() {
			role = "ClusterRole"
			clusterRoles = append(clusterRoles, &rbacv1.ClusterRole{
				ObjectMeta: metav1.ObjectMeta{
					Name:   args.RoleBindingName() + "-" + fmt.Sprint(i),
					Labels: args.Labels(),
				},
				Rules: []rbacv1.PolicyRule{policyRule},
			})

		} else {
			roles = append(roles, &rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{
					Name:      args.RoleBindingName() + "-" + fmt.Sprint(i),
					Namespace: args.Namespace(),
					Labels:    args.Labels(),
				},
				Rules: []rbacv1.PolicyRule{policyRule},
			})
		}

		roleBindings = append(roleBindings, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      args.RoleBindingName() + "-" + fmt.Sprint(i),
				Namespace: roleBindingNamespace,
				Labels:    args.Labels(),
			},
			RoleRef: rbacv1.RoleRef{
				Kind:     role,
				APIGroup: "rbac.authorization.k8s.io",
				Name:     args.RoleName() + "-" + fmt.Sprint(i),
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Namespace: args.Namespace(),
					Name:      args.ServiceAccountName(),
				},
			},
		})

	}

	logger.Info("clusterRoles", "clusterRoles", clusterRoles)
	logger.Info("roles", "roles", roles)
	logger.Info("roleBindings", "roleBindings", roleBindings)
	return clusterRoles, roles, roleBindings
}

func pipelineName(promiseIdentifier, resourceIdentifier, objectName, pipelineName string) string {
	var promiseResource = promiseIdentifier
	if resourceIdentifier != "" {
		promiseResource = fmt.Sprintf("%s-%s", promiseIdentifier, objectName)
	}

	pipelineIdentifier := fmt.Sprintf("kratix-%s-%s", promiseResource, pipelineName)

	return resourceutil.GenerateObjectName(pipelineIdentifier)
}
