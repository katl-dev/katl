package agent

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/katl-dev/katl/internal/generation"
	"github.com/katl-dev/katl/internal/installer/configapply"
	agentapi "github.com/katl-dev/katl/internal/katlc/agentapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) SelectGeneration(ctx context.Context, req *agentapi.GenerationMutationRequest) (*agentapi.GenerationMutationResult, error) {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if err := s.validateGenerationMutation(ctx, req); err != nil {
		return nil, err
	}
	if err := s.mountGenerationBootRoot(ctx); err != nil {
		return nil, err
	}
	err := generation.Select(generation.SelectRequest{
		Root: s.Root, GenerationID: req.GenerationId, OneShot: req.OneShot, Now: s.clock(),
		SetDefault: func(root, entry string) error { return s.SetBootDefault(ctx, root, entry) },
		SetOneshot: func(root, entry string) error { return s.SetBootOneshot(ctx, root, entry) },
	})
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "select generation: %v", err)
	}
	return &agentapi.GenerationMutationResult{GenerationId: req.GenerationId, OneShot: req.OneShot}, nil
}

func (s *Server) RemoveGeneration(ctx context.Context, req *agentapi.GenerationMutationRequest) (*agentapi.GenerationMutationResult, error) {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	if err := s.validateGenerationMutation(ctx, req); err != nil {
		return nil, err
	}
	if req.OneShot {
		return nil, status.Error(codes.InvalidArgument, "oneShot is only valid for boot selection")
	}
	if err := s.mountGenerationBootRoot(ctx); err != nil {
		return nil, err
	}
	if err := generation.Remove(s.Root, req.GenerationId); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "remove generation: %v", err)
	}
	return &agentapi.GenerationMutationResult{GenerationId: req.GenerationId}, nil
}

func (s *Server) validateGenerationMutation(ctx context.Context, req *agentapi.GenerationMutationRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if req == nil {
		return status.Error(codes.InvalidArgument, "request is required")
	}
	if err := cleanPublicID("generationID", req.GenerationId); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.validateMutationTarget(req.ExpectedEnrollmentId, req.ExpectedInventoryNodeName, req.ExpectedMachineId, req.ExpectedCurrentGenerationId); err != nil {
		return err
	}
	ids, err := s.activeOperationIDs()
	if err != nil {
		return status.Errorf(codes.Internal, "read active operations: %v", err)
	}
	if len(ids) > 0 {
		return status.Error(codes.FailedPrecondition, "wait for the active operation to finish before managing generations")
	}
	return nil
}

func (s *Server) mountGenerationBootRoot(ctx context.Context) error {
	if s.MountBootRoot == nil || s.SetBootDefault == nil || s.SetBootOneshot == nil {
		return fmt.Errorf("boot management is not configured")
	}
	return s.MountBootRoot(ctx, filepath.Join(runtimeRoot(s.Root), "efi"))
}

func (s *Server) generationRetention() (generation.Retention, error) {
	current, err := currentGenerationID(s.Root)
	if err != nil {
		return generation.Retention{}, err
	}
	manifest, _, err := configapply.ReadEffectiveGenerationManifest(s.Root, current)
	if err != nil {
		return generation.Retention{}, err
	}
	if manifest.Node.GenerationRetention == nil {
		return generation.Retention{}, nil
	}
	return *manifest.Node.GenerationRetention, nil
}

func (s *Server) pruneGenerations(ctx context.Context) error {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	ids, err := s.activeOperationIDs()
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		return nil
	}
	selection, err := generation.ReadBootSelection(s.Root)
	if err != nil {
		return err
	}
	if selection.PendingHealthValidation || selection.RecoveryRequired {
		return nil
	}
	if readNodeBootHealth(s.Root).State != nodeBootHealthHealthy {
		return nil
	}
	policy, err := s.generationRetention()
	if err != nil {
		return err
	}
	if err := s.mountGenerationBootRoot(ctx); err != nil {
		return err
	}
	removed, err := generation.Prune(s.Root, policy, s.clock())
	if len(removed) > 0 {
		log.Printf("generation cleanup removed %v", removed)
	}
	return err
}

func (s *Server) maintainGenerations(ctx context.Context) {
	// Periodic evaluation ages out generations even when no configuration changes.
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		if err := s.pruneGenerations(runCtx); err != nil && ctx.Err() == nil {
			log.Printf("generation cleanup: %v", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
