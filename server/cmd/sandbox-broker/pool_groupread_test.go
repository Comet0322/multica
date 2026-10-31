package main

import "testing"

func TestLoadPoolWorkspacesFileGroupRead(t *testing.T) {
	s, err := loadPool(envLookup(baseEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if s.workspacesGroupRead {
		t.Fatal("group read must be off unless asked for")
	}
	env := baseEnv()
	env["SANDBOX_WORKSPACES_FILE_GROUP_READ"] = "true"
	if s, err = loadPool(envLookup(env)); err != nil || !s.workspacesGroupRead {
		t.Fatalf("group read not enabled: %v %+v", err, s)
	}
	env["SANDBOX_WORKSPACES_FILE_GROUP_READ"] = "yes please"
	if _, err = loadPool(envLookup(env)); err == nil {
		t.Fatal("a bad value must stop the manager")
	}
}
