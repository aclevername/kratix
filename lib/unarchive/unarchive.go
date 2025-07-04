package unarchive

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/syntasso/kratix/api/v1alpha1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func Unarchive(imageRef string) (string, error) {
	ctx := context.Background()

	username := os.Getenv("OCI_USERNAME")
	password := os.Getenv("OCI_PASSWORD")
	if username == "" || password == "" {
		return "", fmt.Errorf("OCI_USERNAME and OCI_PASSWORD must be set")
	}

	tempDir, err := os.MkdirTemp("", "oci-unarchive-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return "", fmt.Errorf("failed to create repo ref: %w", err)
	}
	repo.PlainHTTP = strings.HasPrefix(imageRef, "http://")
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Credential: func(ctx context.Context, host string) (auth.Credential, error) {
			return auth.Credential{
				Username: username,
				Password: password,
			}, nil
		},
		Header: make(map[string][]string),
	}

	mem := memory.New()
	tag := "latest"
	manifestDesc, err := oras.Copy(ctx, repo, tag, mem, tag, oras.DefaultCopyOptions)
	if err != nil {
		return "", fmt.Errorf("failed to pull image: %w", err)
	}

	manifestReader, err := mem.Fetch(ctx, manifestDesc)
	if err != nil {
		return "", fmt.Errorf("failed to fetch manifest blob: %w", err)
	}
	defer manifestReader.Close()

	manifestBlob, err := io.ReadAll(manifestReader)
	if err != nil {
		return "", fmt.Errorf("failed to read manifest blob: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBlob, &manifest); err != nil {
		return "", fmt.Errorf("failed to unmarshal manifest: %w", err)
	}

	for _, layer := range manifest.Layers {
		layerReader, err := mem.Fetch(ctx, layer)
		if err != nil {
			return "", fmt.Errorf("failed to fetch layer: %w", err)
		}
		defer layerReader.Close()

		tarReader := tar.NewReader(layerReader)
		for {
			hdr, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("tar read error: %w", err)
			}
			if hdr.Typeflag == tar.TypeDir {
				continue
			}
			outPath := filepath.Join(tempDir, hdr.Name)
			if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
				return "", fmt.Errorf("mkdir error: %w", err)
			}
			file, err := os.Create(outPath)
			if err != nil {
				return "", fmt.Errorf("file create error: %w", err)
			}
			if _, err := io.Copy(file, tarReader); err != nil {
				file.Close()
				return "", fmt.Errorf("file write error: %w", err)
			}
			file.Close()
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
