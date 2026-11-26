package services

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"

	"nexora/internal/updates"
)

// UpdateService tells the UI when a newer Nexora is released.
type UpdateService struct {
	checker *updates.Checker // nil when update checks are off
}

func NewUpdateService(checker *updates.Checker) *UpdateService {
	return &UpdateService{checker: checker}
}

func (s *UpdateService) ServiceStartup(context.Context, application.ServiceOptions) error {
	if s.checker != nil {
		s.checker.Start()
	}
	return nil
}

func (s *UpdateService) ServiceShutdown() error {
	if s.checker != nil {
		s.checker.Close()
	}
	return nil
}

// Info is the last check's result.
func (s *UpdateService) Info() updates.Info {
	if s.checker == nil {
		return updates.Info{}
	}
	return s.checker.Info()
}

// Check asks for the latest release now.
func (s *UpdateService) Check(ctx context.Context) updates.Info {
	if s.checker == nil {
		return updates.Info{}
	}
	return s.checker.Check(ctx)
}
