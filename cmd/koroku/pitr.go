package main

import (
	"context"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/store"
)

// statusReporters passes the status changes of deployments to several
// reporters.
type statusReporters []deploy.StatusReporter

func (rs statusReporters) Report(change deploy.StatusChange) {
	for _, r := range rs {
		if r != nil {
			r.Report(change)
		}
	}
}

// pitrReporter starts the preparation of point-in-time recovery after a
// deployment succeeded (ARCHITECTURE 10.9). The service is set once the
// engine it needs exists.
type pitrReporter struct {
	store   *store.Store
	service *pitr.Service
}

// RequestBase implements deploy.PITRHooks.
func (r *pitrReporter) RequestBase(appID int64) {
	if r.service != nil {
		r.service.RequestBase(appID)
	}
}

func (r *pitrReporter) Report(change deploy.StatusChange) {
	if r.service == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d, err := r.store.GetDeployment(ctx, change.DeploymentID)
		if err != nil || d.Status != store.StatusSucceeded {
			return
		}
		r.service.DeploymentSucceeded(d.AppID)
	}()
}
