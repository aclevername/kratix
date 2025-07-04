package writers

//go:generate go run github.com/maxbrunsfeld/counterfeiter/v6 -generate

import (
	"fmt"
)

//counterfeiter:generate . StateStoreWriter
type StateStoreWriter interface {
	UpdateFiles(subDir string, workPlacementName string, image string, workloadsToDelete []string) (string, error)
	ReadFile(filename string) ([]byte, error)
	ValidatePermissions() error
}

var ErrFileNotFound = fmt.Errorf("file not found")
