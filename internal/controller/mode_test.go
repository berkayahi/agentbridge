package controller

import (
	"context"
	"errors"
	"testing"
)

func TestActivateRefusesModeSwitchWithActiveExecution(t *testing.T) {
	store, err := NewFileModeStore(t.TempDir() + "/mode.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(context.Background(), store, ModeStandalone); err != nil {
		t.Fatal(err)
	}
	if _, err := SetActiveExecution(context.Background(), store, "execution-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(context.Background(), store, ModeManaged); !errors.Is(err, ErrActiveExecution) {
		t.Fatalf("Activate() error = %v, want ErrActiveExecution", err)
	}
	state, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeStandalone || state.ActiveExecutionID != "execution-1" {
		t.Fatalf("state = %#v, want standalone with active execution", state)
	}
}

func TestActivatePersistsExplicitMode(t *testing.T) {
	path := t.TempDir() + "/mode.json"
	store, err := NewFileModeStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(context.Background(), store, ModeManaged); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewFileModeStore(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reloaded.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != ModeManaged {
		t.Fatalf("mode = %q, want managed", state.Mode)
	}
}
