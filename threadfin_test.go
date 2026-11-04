package main

import "testing"

// Automatic updates download releases from the configured repository. If
// that is still upstream Threadfin, a newer upstream release would replace
// this build and undo its changes.
func TestAutoUpdateDoesNotPointAtUpstream(t *testing.T) {
	if GitHub.Update && GitHub.User == "Threadfin" && GitHub.Repo == "Threadfin" {
		t.Fatal("automatic updates are on and point at upstream Threadfin: set GitHub.User/Repo to this project's repository first")
	}
}
