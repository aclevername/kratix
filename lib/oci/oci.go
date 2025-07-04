package oci

import (
	"fmt"
	"os"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func Push(dir, imageRef string) error {
	img, err := tarball.ImageFromPath(dir, nil)
	if err != nil {
		return fmt.Errorf("failed to create image from path: %w", err)
	}

	username := os.Getenv("OCI_USERNAME")
	password := os.Getenv("OCI_PASSWORD")
	if username == "" || password == "" {
		return fmt.Errorf("OCI_USERNAME and OCI_PASSWORD must be set")
	}

	auth := &authn.Basic{
		Username: username,
		Password: password,
	}

	if err := crane.Push(img, imageRef, crane.WithAuth(auth)); err != nil {
		return fmt.Errorf("failed to push image: %w", err)
	}

	return nil
}
