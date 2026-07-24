package external

import (
	"context"
	"fmt"
	"time"
)

type BundleReconciler struct {
	repository BundleRepository
	store      BundleObjectStore
	config     BundleServiceConfig
	now        func() time.Time
}

func NewBundleReconciler(service *BundleService) (*BundleReconciler, error) {
	if service == nil || service.repository == nil || service.store == nil {
		return nil, fmt.Errorf("bundle service is required for reconciliation")
	}
	return &BundleReconciler{repository: service.repository, store: service.store, config: service.config, now: time.Now}, nil
}

func (reconciler *BundleReconciler) ReconcileOnce(ctx context.Context) (bool, error) {
	if reconciler == nil || reconciler.repository == nil || reconciler.store == nil {
		return false, fmt.Errorf("bundle reconciler is not configured")
	}
	now := reconciler.now().UTC()
	leaseToken, err := generateExternalID(reconciler.config.Random)
	if err != nil {
		return false, err
	}
	claim, claimed, err := reconciler.repository.ClaimNextBundlePublication(ctx, leaseToken, now, now.Add(reconciler.config.PublicationLease))
	if err != nil {
		return false, err
	}
	if !claimed {
		swept, err := reconciler.repository.SweepUnrecoverableBundlePublications(ctx, now.Add(-reconciler.config.PendingAbandonAfter), 100)
		return swept > 0, err
	}
	promotionContext, cancelPromotion := context.WithTimeout(ctx, reconciler.config.PublicationLease/2)
	defer cancelPromotion()
	if err := reconciler.store.Promote(promotionContext, claim.StagingKey, claim.ObjectKey, claim.SizeBytes, claim.RequestHash); err != nil {
		nextAttempt := now.Add(reconciler.config.PublicationRetry)
		abandoned, recordErr := reconciler.repository.FailBundlePublication(ctx, claim, "OBJECT_PROMOTION_FAILED", nextAttempt, reconciler.config.MaxPublishAttempts)
		if recordErr != nil {
			return true, fmt.Errorf("promote bundle: %v; record failure: %w", err, recordErr)
		}
		if abandoned {
			maintenanceContext, cancelMaintenance := reconciler.maintenanceContext(ctx)
			_ = reconciler.store.Discard(maintenanceContext, claim.StagingKey)
			cancelMaintenance()
		}
		return true, fmt.Errorf("promote bundle: %w", err)
	}
	if err := reconciler.repository.CompleteBundlePublication(ctx, claim, reconciler.now().UTC()); err != nil {
		return true, fmt.Errorf("complete reconciled bundle publication: %w", err)
	}
	maintenanceContext, cancelMaintenance := reconciler.maintenanceContext(ctx)
	_ = reconciler.store.Discard(maintenanceContext, claim.StagingKey)
	cancelMaintenance()
	return true, nil
}

func (reconciler *BundleReconciler) maintenanceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, reconciler.config.MaintenanceTimeout)
}

func (reconciler *BundleReconciler) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("bundle reconciliation interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := reconciler.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
			// Durable state retains the retry schedule; the next tick resumes.
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
