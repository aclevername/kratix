package main

import "testing"

func TestParseEnabledControllersDefaultsToAll(t *testing.T) {
	controllers, err := parseEnabledControllers("")
	if err != nil {
		t.Fatalf("parseEnabledControllers returned an unexpected error: %v", err)
	}

	if len(controllers) != len(supportedControllers) {
		t.Fatalf("expected %d controllers but got %d", len(supportedControllers), len(controllers))
	}
}

func TestParseEnabledControllersParsesSelection(t *testing.T) {
	controllers, err := parseEnabledControllers("work,promise")
	if err != nil {
		t.Fatalf("parseEnabledControllers returned an unexpected error: %v", err)
	}

	if len(controllers) != 2 {
		t.Fatalf("expected 2 controllers but got %d", len(controllers))
	}
	if !controllers.enabled(controllerNameWork) {
		t.Fatalf("expected %q to be enabled", controllerNameWork)
	}
	if !controllers.enabled(controllerNamePromise) {
		t.Fatalf("expected %q to be enabled", controllerNamePromise)
	}
}

func TestParseEnabledControllersRejectsAllWithOthers(t *testing.T) {
	_, err := parseEnabledControllers("all,work")
	if err == nil {
		t.Fatalf("expected parseEnabledControllers to return an error when all is combined with other controllers")
	}
}

func TestParseEnabledControllersRejectsUnknownController(t *testing.T) {
	_, err := parseEnabledControllers("unknown-controller")
	if err == nil {
		t.Fatalf("expected parseEnabledControllers to return an error for unknown controller")
	}
}
