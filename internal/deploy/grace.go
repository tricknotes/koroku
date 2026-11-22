package deploy

import (
	"context"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/store"
)

// logStopGraces explains, at the start step, how long replacing the running
// containers may take (ARCHITECTURE 6): Docker stops each container with the
// grace period it was created with, and Compose stops dependent services
// one after the other, so the waits may add up. next is the project about to
// start; its longer grace periods apply the next time its containers stop.
// Nothing is written when every grace period is Docker's default. Failing to
// read the containers is not a reason to stop the deployment.
func (e *Engine) logStopGraces(ctx context.Context, app store.App, next compose.Project, log *deployLog) {
	if running := e.runningStopGraces(ctx, app); len(running) > 0 {
		log.Printf("Stopping the running containers may wait up to %s: %s",
			compose.TotalGrace(running), compose.DescribeGraces(running))
	}
	if long := next.LongStopGracePeriods(); len(long) > 0 {
		log.Printf("This release sets a longer stop grace period: %s", compose.DescribeGraces(long))
	}
}

// runningStopGraces returns, per service, the longest stop grace period of
// the app's running containers when it is longer than Docker's default.
// Replicas of one service stop at the same time, so the longest counts.
func (e *Engine) runningStopGraces(ctx context.Context, app store.App) []compose.ServiceGrace {
	ids, err := e.projectContainers(ctx, app)
	if err != nil || len(ids) == 0 {
		if err != nil {
			e.opts.Logger.Debug("listing containers for their stop grace periods failed", "app", app.Slug, "error", err)
		}
		return nil
	}
	containers, err := e.inspectForView(ctx, ids)
	if err != nil {
		e.opts.Logger.Debug("inspecting containers for their stop grace periods failed", "app", app.Slug, "error", err)
		return nil
	}
	longest := map[string]time.Duration{}
	var order []string
	for _, c := range containers {
		if c.State.Status != "running" || c.Config.StopTimeout == nil {
			continue
		}
		d := time.Duration(*c.Config.StopTimeout) * time.Second
		service := c.Config.Labels[labelService]
		if service == "" || d <= compose.DefaultStopGracePeriod {
			continue
		}
		if _, seen := longest[service]; !seen {
			order = append(order, service)
		}
		longest[service] = max(longest[service], d)
	}
	out := make([]compose.ServiceGrace, 0, len(order))
	for _, service := range order {
		out = append(out, compose.ServiceGrace{Service: service, Period: longest[service]})
	}
	return out
}
