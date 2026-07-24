package standalone

import (
	"errors"

	"github.com/berkayahi/agentbridge/internal/workmodel"
)

var ErrTaskOwnedByAnotherController = errors.New("app: task is owned by another controller")

// Empty ownership is accepted for in-memory and pre-ownership test stores.
// Every persisted v2 task is assigned explicitly by its creating authority or
// the migration default before it can reach production reconciliation.
func standaloneOwnsTask(value workmodel.Task) bool {
	return value.ControllerOwner == "" || value.ControllerOwner == workmodel.TaskControllerStandalone
}

func requireStandaloneTask(value workmodel.Task) error {
	if !standaloneOwnsTask(value) {
		return ErrTaskOwnedByAnotherController
	}
	return nil
}
