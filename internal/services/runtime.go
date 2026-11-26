package services

import (
	"context"

	"nexora/internal/noderuntime"
)

// RuntimeService reports the Node.js runtime that runs JS extensions and
// downloads the managed one when there is none.
type RuntimeService struct {
	node *noderuntime.Runtime
}

func NewRuntimeService(node *noderuntime.Runtime) *RuntimeService {
	return &RuntimeService{node: node}
}

// Status is the runtime's state, including download progress.
func (s *RuntimeService) Status() noderuntime.Status { return s.node.Status() }

// Download installs the pinned Node.js in the background; poll Status for
// progress. Extensions that failed for lack of it restart once it's ready.
func (s *RuntimeService) Download() error { return s.node.Download() }

// Cancel stops a running download.
func (s *RuntimeService) Cancel() { s.node.Cancel() }

// Recheck looks for Node.js again, e.g. after the user installed it.
func (s *RuntimeService) Recheck(ctx context.Context) noderuntime.Status {
	return s.node.Resolve(ctx)
}
