package lib

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/internal/telemetry"
	"github.com/syntasso/kratix/lib/compression"
	"github.com/syntasso/kratix/lib/hash"
	"github.com/syntasso/kratix/lib/objectutil"
	"github.com/syntasso/kratix/lib/resourceutil"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	sigsYaml "sigs.k8s.io/yaml"
)

const (
	compoundRequestsDirectoryName = "compound-requests"
	compoundRequestsSource        = "compound-requests"
)

func (w *WorkCreator) createCompoundRequestsWork(
	ctx context.Context,
	rootDirectory,
	promiseName,
	namespace,
	resourceName,
	resourceNamespace,
	workflowType,
	pipelineName,
	traceParent,
	traceState string,
	logger logr.Logger,
) error {
	compoundRequestsDir := filepath.Join(rootDirectory, compoundRequestsDirectoryName)
	if _, err := os.Stat(compoundRequestsDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			logger.Info("no compound requests directory found, skipping compound requests work creation", "directory", compoundRequestsDir)
			return nil
		}
		return fmt.Errorf("stat compound request directory %q: %w", compoundRequestsDir, err)
	}

	generation, err := readGeneration(filepath.Join(rootDirectory, "res", "object.yaml"))
	if err != nil {
		return err
	}

	workloads, resReqRef, err := buildCompoundRequestWorkloads(compoundRequestsDir, generation)
	if err != nil {
		return err
	}

	if len(workloads) == 0 {
		return nil
	}

	identifier := BuildWorkIdentifier(promiseName, resourceName, resourceNamespace, pipelineName, workflowType)

	compoundLogger := logger.WithValues("compoundRequests", true, "compoundIdentifier", identifier, "compoundPipelineName", pipelineName)
	compoundLogger.Info("creating compound requests work")

	compoundRequestMetadata := &v1alpha1.CompoundRequestMetadata{}
	compoundRequestMetadata.Name = identifier
	compoundRequestMetadata.Namespace = namespace
	compoundRequestMetadata.Spec.Resources = resReqRef
	// create compound request metadata, or update if it already exists
	existingCompoundRequestMetadata := &v1alpha1.CompoundRequestMetadata{}
	err = w.K8sClient.Get(ctx, types.NamespacedName{Name: compoundRequestMetadata.Name, Namespace: compoundRequestMetadata.Namespace}, existingCompoundRequestMetadata)
	if err != nil {
		if !kerrors.IsNotFound(err) {
			return fmt.Errorf("getting compound request metadata: %w", err)
		}

		if err := w.K8sClient.Create(ctx, compoundRequestMetadata); err != nil {
			return fmt.Errorf("creating compound request metadata: %w", err)
		}
		compoundLogger.Info("compound request metadata created", "compoundRequestMetadataName", compoundRequestMetadata.Name)
	} else {
		existingCompoundRequestMetadata.Spec = compoundRequestMetadata.Spec
		if err := w.K8sClient.Update(ctx, existingCompoundRequestMetadata); err != nil {
			return fmt.Errorf("updating compound request metadata: %w", err)
		}
		compoundLogger.Info("compound request metadata updated", "compoundRequestMetadataName", existingCompoundRequestMetadata.Name)
	}

	work := &v1alpha1.Work{}
	work.Name = objectutil.GenerateObjectName(identifier)
	work.Namespace = namespace
	work.Spec.PromiseName = promiseName
	work.Spec.ResourceName = resourceName
	work.Spec.WorkloadGroups = []v1alpha1.WorkloadGroup{
		{
			ID:        hash.ComputeHash(compoundRequestsDirectoryName),
			Directory: compoundRequestsDirectoryName,
			Workloads: workloads,
			DestinationSelectors: []v1alpha1.WorkloadGroupScheduling{
				{
					MatchLabels: map[string]string{"environment": "platform"},
					Source:      compoundRequestsSource,
				},
			},
		},
	}
	work.SetAnnotations(telemetry.ApplyTraceAnnotations(work.GetAnnotations(), traceParent, traceState))
	work.Labels = map[string]string{}

	if !strings.HasPrefix(workflowType, string(v1alpha1.WorkflowTypeResource)) {
		work.Labels = v1alpha1.GenerateSharedLabelsForPromise(promiseName)
	}

	workLabels := resourceutil.GetWorkLabels(promiseName, resourceName, resourceNamespace, pipelineName, workflowType)
	work.SetLabels(labels.Merge(work.GetLabels(), workLabels))

	existingWork, err := resourceutil.GetWork(w.K8sClient, namespace, work.GetLabels())
	if err != nil {
		return err
	}

	if existingWork == nil {
		if err := w.K8sClient.Create(ctx, work); err != nil {
			return fmt.Errorf("creating compound requests work: %w", err)
		}
		compoundLogger.Info("compound requests work created", "workName", work.Name)
		return nil
	}

	existingWork.Spec = work.Spec
	existingWork.SetAnnotations(telemetry.ApplyTraceAnnotations(existingWork.GetAnnotations(), traceParent, traceState))

	if err := w.K8sClient.Update(ctx, existingWork); err != nil {
		return fmt.Errorf("updating compound requests work: %w", err)
	}
	compoundLogger.Info("compound requests work updated", "workName", existingWork.Name)

	return nil
}

func buildCompoundRequestWorkloads(root, generation string) ([]v1alpha1.Workload, []v1alpha1.ResReqRef, error) {
	var workloads []v1alpha1.Workload
	compoundRequestMedata := []v1alpha1.ResReqRef{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading compound request file %q: %w", path, err)
		}

		updatedContents, resReqRef, err := applyGenerationLabel(content, generation)
		if err != nil {
			return err
		}

		compoundRequestMedata = append(compoundRequestMedata, resReqRef...)

		compressed, err := compression.CompressContent(updatedContents)
		if err != nil {
			return fmt.Errorf("compressing compound request file %q: %w", path, err)
		}

		relativePath, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %q: %w", path, err)
		}

		workloads = append(workloads, v1alpha1.Workload{
			Filepath: relativePath,
			Content:  string(compressed),
		})
		return nil
	})

	if err != nil {
		return nil, compoundRequestMedata, fmt.Errorf("walking compound requests directory %q: %w", root, err)
	}

	return workloads, compoundRequestMedata, nil
}

func applyGenerationLabel(contents []byte, generation string) ([]byte, []v1alpha1.ResReqRef, error) {
	reader := bytes.NewReader(contents)
	decoder := yamlutil.NewYAMLOrJSONDecoder(reader, 4096)

	var documents [][]byte

	resReqRef := []v1alpha1.ResReqRef{}

	for {
		var document map[string]interface{}
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("decoding manifest: %w", err)
		}

		if len(document) == 0 {
			continue
		}

		resReqRef = append(resReqRef, v1alpha1.ResReqRef{
			Name:       document["metadata"].(map[string]interface{})["name"].(string),
			Namespace:  document["metadata"].(map[string]interface{})["namespace"].(string),
			APIVersion: document["apiVersion"].(string),
			Kind:       document["kind"].(string),
		})

		withGeneration := ensureGenerationLabel(document, generation)

		marshalled, err := sigsYaml.Marshal(withGeneration)
		if err != nil {
			return nil, nil, fmt.Errorf("encoding manifest with generation label: %w", err)
		}
		documents = append(documents, marshalled)
	}

	if len(documents) == 0 {
		return contents, nil, nil
	}

	return bytes.Join(documents, []byte("---\n")), resReqRef, nil
}

func ensureGenerationLabel(document map[string]interface{}, generation string) map[string]interface{} {
	metadata, ok := document["metadata"].(map[string]interface{})
	if !ok || metadata == nil {
		metadata = map[string]interface{}{}
	}

	labelsMap, ok := metadata["labels"].(map[string]interface{})
	if !ok || labelsMap == nil {
		labelsMap = map[string]interface{}{}
	}

	labelsMap["kratix.io/generation"] = generation
	metadata["labels"] = labelsMap
	document["metadata"] = metadata
	return document
}

func readGeneration(objectFilePath string) (string, error) {
	data, err := os.ReadFile(objectFilePath)
	if err != nil {
		return "", fmt.Errorf("reading object file %q: %w", objectFilePath, err)
	}

	var parsed struct {
		Metadata struct {
			Generation int64 `yaml:"generation"`
		} `yaml:"metadata"`
	}

	if err := sigsYaml.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("parsing object file %q: %w", objectFilePath, err)
	}

	return strconv.FormatInt(parsed.Metadata.Generation, 10), nil
}
