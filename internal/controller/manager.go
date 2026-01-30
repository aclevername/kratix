package controller

import "sigs.k8s.io/controller-runtime/pkg/manager"

// Manager is kept for compatibility with generated fakes in tests.
type Manager interface {
	manager.Manager
}
