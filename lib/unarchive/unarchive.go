package unarchive

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/syntasso/kratix/api/v1alpha1"
)

func Unarchive(imageRef string) (string, error) {
	tempDir, err := os.MkdirTemp("", "oci-unarchive-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	img, err := crane.Pull(imageRef)
	if err != nil {
		return "", fmt.Errorf("failed to pull image: %w", err)
	}

	layers, err := img.Layers()
	if err != nil {
		return "", fmt.Errorf("failed to get image layers: %w", err)
	}

	for _, layer := range layers {
		reader, err := layer.Uncompressed() // Get uncompressed contents of the layer
		if err != nil {
			return "", fmt.Errorf("failed to uncompress layer: %w", err)
		}
		defer reader.Close()

		tarReader := tar.NewReader(reader)
		for {
			header, err := tarReader.Next()
			if err == io.EOF {
				break // End of archive
			}
			if err != nil {
				return "", fmt.Errorf("failed to read tar header: %w", err)
			}

			if header.Typeflag == tar.TypeDir {
				continue
			}

			filePath := filepath.Join(tempDir, header.Name)
			if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
				return "", fmt.Errorf("failed to create directory for file: %w", err)
			}

			file, err := os.Create(filePath)
			if err != nil {
				return "", fmt.Errorf("failed to create file: %w", err)
			}
			defer file.Close()

			if _, err := io.Copy(file, tarReader); err != nil {
				return "", fmt.Errorf("failed to write file content: %w", err)
			}
		}
	}

	return tempDir, nil
}

func GetWorkloadsFromImage(imageRef string) ([]v1alpha1.Workload, error) {
	tempDir, err := Unarchive(imageRef)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	var workloads []v1alpha1.Workload
	err = filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(tempDir, path)
			if err != nil {
				return err
			}
			workloads = append(workloads, v1alpha1.Workload{
				Filepath: relPath,
				Content:  string(content),
			})
		}
		return nil
	})

	if err != nil {
		return nil, err
	}

	return workloads, nil
}
