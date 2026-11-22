package compose

import (
	"strings"
	"testing"
	"time"
)

func graceProject(periods map[string]string) Project {
	p := Project{Services: map[string]Service{}}
	for name, period := range periods {
		p.Services[name] = Service{Image: "example/app:1", StopGracePeriodRaw: period}
	}
	return p
}

func TestStopGracePeriodLimit(t *testing.T) {
	params := PolicyParams{WebService: "web"}
	cases := []struct {
		name    string
		periods map[string]string
		reject  bool
	}{
		{"defaults", map[string]string{"web": "", "worker": ""}, false},
		{"one service at the limit", map[string]string{"web": "", "scheduler": "10m0s"}, false},
		{"one service above the limit", map[string]string{"web": "", "scheduler": "10m1s"}, true},
		// Compose stops dependent services one after the other, so the
		// total counts, not the largest.
		{"total above the limit", map[string]string{"web": "30s", "scheduler": "6m0s", "worker": "4m0s"}, true},
		{"total at the limit", map[string]string{"web": "1m0s", "scheduler": "5m0s", "worker": "4m0s"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, relax := range []bool{false, true} {
				params.Relax = relax
				r := CheckPolicy(graceProject(tc.periods), params)
				var found bool
				for _, v := range r.Violations {
					if v.Rule == RuleStopGracePeriod {
						found = true
					}
				}
				if found != tc.reject {
					t.Errorf("relax=%v: rejected = %v, want %v (%v)", relax, found, tc.reject, r.Violations)
				}
				for _, v := range r.Waived {
					if v.Rule == RuleStopGracePeriod {
						t.Errorf("the stop grace limit must never be waived: %s", v)
					}
				}
			}
		})
	}
}

func TestStopGracePeriodViolationNamesTheServices(t *testing.T) {
	r := CheckPolicy(graceProject(map[string]string{"web": "", "scheduler": "8m0s", "worker": "3m0s"}), PolicyParams{WebService: "web"})
	if len(r.Violations) != 1 {
		t.Fatalf("violations = %v", r.Violations)
	}
	msg := r.Violations[0].String()
	for _, want := range []string{"[stop_grace_period]", "11m0s", "scheduler 8m0s, worker 3m0s", "10m0s"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestLongStopGracePeriods(t *testing.T) {
	p := graceProject(map[string]string{"web": "", "worker": "30s", "db": "10s", "scheduler": "1m30s", "odd": "bogus"})
	got := p.LongStopGracePeriods()
	if DescribeGraces(got) != "scheduler 1m30s, worker 30s" {
		t.Errorf("LongStopGracePeriods = %v", got)
	}
	if TotalGrace(got) != 2*time.Minute {
		t.Errorf("TotalGrace = %s", TotalGrace(got))
	}
	if p.TotalStopGracePeriod() != 2*time.Minute+10*time.Second {
		t.Errorf("TotalStopGracePeriod = %s", p.TotalStopGracePeriod())
	}
}
