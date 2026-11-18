package routing

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/domains"
	"github.com/tricknotes/koroku/internal/store"
)

// RecheckInterval is how often pending domains are verified again.
const RecheckInterval = 5 * time.Minute

// Syncer reloads the proxy. *Router implements it.
type Syncer interface {
	Sync(ctx context.Context) error
}

// DomainChecker verifies custom domains against DNS and keeps the proxy in
// step when a domain becomes active.
type DomainChecker struct {
	Store    *store.Store
	Verifier domains.Verifier
	// Router is optional; without it only the database is updated.
	Router Syncer
	Clock  clock.Clock
	Logger *slog.Logger
}

func (c *DomainChecker) logger() *slog.Logger {
	if c.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Logger
}

// Check verifies one domain now, stores the outcome and returns the updated
// row. Only inactive hostnames are kept away from the proxy: a hostname whose
// DNS does not point here would make it retry certificate issuance forever.
func (c *DomainChecker) Check(ctx context.Context, d store.Domain) (store.Domain, error) {
	err := c.Verifier.Verify(ctx, d.Hostname)
	if errors.Is(err, context.Canceled) {
		// The caller went away; this says nothing about the domain.
		return d, err
	}
	message := ""
	if err != nil {
		message = err.Error()
	}
	if err := c.Store.RecordDomainCheck(ctx, d.ID, err == nil, message, c.Clock.Now()); err != nil {
		return d, err
	}
	updated, err := c.Store.GetDomain(ctx, d.AppID, d.ID)
	if err != nil {
		return d, err
	}
	if updated.Status != d.Status || updated.IsPrimary != d.IsPrimary {
		c.logger().Info("domain status changed", "hostname", updated.Hostname, "status", updated.Status)
		c.sync(ctx)
	}
	return updated, nil
}

// CheckPending verifies every pending domain once.
func (c *DomainChecker) CheckPending(ctx context.Context) {
	pending, err := c.Store.ListPendingDomains(ctx)
	if err != nil {
		c.logger().Error("listing pending domains failed", "error", err)
		return
	}
	for _, d := range pending {
		if ctx.Err() != nil {
			return
		}
		if _, err := c.Check(ctx, d); err != nil && !errors.Is(err, context.Canceled) {
			c.logger().Error("checking domain failed", "hostname", d.Hostname, "error", err)
		}
	}
}

// Run re-verifies pending domains every interval until ctx is canceled. Each
// round also syncs the proxy, which repairs it after Caddy was unreachable.
func (c *DomainChecker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = RecheckInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.CheckPending(ctx)
			c.sync(ctx)
		}
	}
}

func (c *DomainChecker) sync(ctx context.Context) {
	if c.Router == nil {
		return
	}
	if err := c.Router.Sync(ctx); err != nil {
		c.logger().Error("syncing routes failed", "error", err)
	}
}
