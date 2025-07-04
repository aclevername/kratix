package oci

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func Push(path, imageRef string) error {
	ctx := context.Background()

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("failed to stat path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("expected directory input, got file")
	}

	username := os.Getenv("OCI_USERNAME")
	password := os.Getenv("OCI_PASSWORD")
	if username == "" || password == "" {
		return fmt.Errorf("OCI_USERNAME and OCI_PASSWORD must be set")
	}
	fmt.Printf("Using OCI credentials from environment: user=%s\n", username)

	repo, err := remote.NewRepository(imageRef)
	if err != nil {
		return fmt.Errorf("failed to create remote repo reference: %w", err)
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

	store := memory.New()
	var layers []ocispec.Descriptor

	err = filepath.Walk(path, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}

		fmt.Printf("Adding file to push: %s\n", relPath)

		data, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("failed to read file %s: %w", relPath, err)
		}

		desc := ocispec.Descriptor{
			MediaType: "application/x-yaml",
			Digest:    digest.FromBytes(data),
			Size:      int64(len(data)),
			Annotations: map[string]string{
				ocispec.AnnotationTitle: relPath,
			},
		}

		if err := store.Push(ctx, desc, bytes.NewReader(data)); err != nil {
			return fmt.Errorf("failed to push file to store: %w", err)
		}

		layers = append(layers, desc)
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking path: %w", err)
	}

	config := []byte("{}")
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(config),
		Size:      int64(len(config)),
	}
	if err := store.Push(ctx, configDesc, bytes.NewReader(config)); err != nil {
		return fmt.Errorf("failed to push config blob: %w", err)
	}

	manifestDesc, err := oras.PackManifest(
		ctx,
		store,
		oras.PackManifestVersion1_1_RC4,
		"application/vnd.kratix.bundle.v1",
		oras.PackManifestOptions{
			Layers:           layers,
			ConfigDescriptor: &configDesc,
			ManifestAnnotations: map[string]string{
				ocispec.AnnotationTitle:   "kratix-workload-bundle",
				ocispec.AnnotationCreated: "2025-07-04T12:00:00Z", // optional: make reproducible
			},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to pack manifest: %w", err)
	}

	fmt.Printf("Pushing to %s\n", imageRef)
	tag := "latest"
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		return fmt.Errorf("failed to tag manifest in memory store: %w", err)
	}

	_, err = oras.Copy(ctx, store, tag, repo, tag, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("failed to push to remote: %w", err)
	}
	if err != nil {
		return fmt.Errorf("failed to push to remote: %w", err)
	}

	fmt.Printf("✅ Push complete with manifest digest: %s\n", manifestDesc.Digest)
	return nil
}
