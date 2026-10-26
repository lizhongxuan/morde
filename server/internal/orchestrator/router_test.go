package orchestrator

import (
	"errors"
	"reflect"
	"testing"

	"morde/server/internal/app"
)

type sandboxClientStub struct{ app.SandboxClient }

func TestRouter(t *testing.T) {
	client := &sandboxClientStub{}
	input := map[string]app.SandboxClient{"vm": client, "container": client}
	router := NewRouter(input)
	delete(input, "vm")
	if got, want := router.Profiles(), []string{"container", "vm"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Profiles() = %v, want %v", got, want)
	}
	got, err := router.For("container")
	if err != nil || got != client {
		t.Fatalf("For(container) = %v, %v", got, err)
	}
	if _, err := router.For("missing"); !errors.Is(err, app.ErrSandboxUnsupported) {
		t.Fatalf("For(missing) error = %v", err)
	}
}
