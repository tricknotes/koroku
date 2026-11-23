package compose

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// DefaultStopGracePeriod is how long Docker waits for a container to stop
// after SIGTERM when the service sets no stop_grace_period.
const DefaultStopGracePeriod = 10 * time.Second

// MaxStopGracePeriod bounds the total of the stop_grace_period of an app's
// services (ARCHITECTURE 6). Compose stops dependent services one after the
// other, so their waits add up; the deploy worker is global and serial, and
// koroku's timeouts for the operations that stop containers include this
// much.
const MaxStopGracePeriod = 10 * time.Minute

// RuleStopGracePeriod rejects a project whose stop grace periods add up to
// more than MaxStopGracePeriod. The relax flag does not waive it: it guards
// other apps' deployments and koroku's own timeouts, not this app's
// isolation.
const RuleStopGracePeriod = "stop_grace_period"

// ServiceGrace is the stop grace period of one service (or container).
type ServiceGrace struct {
	Service string
	Period  time.Duration
}

// StopGracePeriod returns the service's stop grace period and whether it set
// one. A value that does not parse (Compose validates the field, so this
// does not happen) counts as not set.
func (s Service) StopGracePeriod() (time.Duration, bool) {
	if s.StopGracePeriodRaw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s.StopGracePeriodRaw)
	if err != nil || d < 0 {
		return 0, false
	}
	return d, true
}

// LongStopGracePeriods lists the services whose stop grace period is longer
// than Docker's default, by name.
func (p Project) LongStopGracePeriods() []ServiceGrace {
	var out []ServiceGrace
	for _, name := range p.ServiceNames() {
		if d, ok := p.Services[name].StopGracePeriod(); ok && d > DefaultStopGracePeriod {
			out = append(out, ServiceGrace{Service: name, Period: d})
		}
	}
	return out
}

// TotalStopGracePeriod adds up the stop grace periods the services set.
// Services without one are not counted: koroku's timeouts leave room for
// Docker's default.
func (p Project) TotalStopGracePeriod() time.Duration {
	var total time.Duration
	for _, svc := range p.Services {
		if d, ok := svc.StopGracePeriod(); ok {
			total += d
		}
	}
	return total
}

// TotalGrace adds up grace periods.
func TotalGrace(graces []ServiceGrace) time.Duration {
	var total time.Duration
	for _, g := range graces {
		total += g.Period
	}
	return total
}

// DescribeGraces renders grace periods for the deploy log, sorted by
// service: "scheduler 5m0s, worker 30s".
func DescribeGraces(graces []ServiceGrace) string {
	sorted := slices.Clone(graces)
	slices.SortStableFunc(sorted, func(a, b ServiceGrace) int { return strings.Compare(a.Service, b.Service) })
	parts := make([]string, len(sorted))
	for i, g := range sorted {
		parts[i] = fmt.Sprintf("%s %s", g.Service, g.Period)
	}
	return strings.Join(parts, ", ")
}
