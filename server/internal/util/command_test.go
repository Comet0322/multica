package util

import (
	"context"
	"reflect"
	"testing"
)

func TestCommandArgv(t *testing.T) {
	cmd := Command("echo", "a", "b")
	if want := []string{"echo", "a", "b"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("Args = %v, want %v", cmd.Args, want)
	}
	out, err := CommandContext(context.Background(), "echo", "x", "y").Output()
	if err != nil || string(out) != "x y\n" {
		t.Fatalf("output = %q, %v", out, err)
	}
}
