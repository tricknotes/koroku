package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

const mib = 1 << 20

func i64(v int64) *int64 { return &v }

// buckets makes n consecutive 5-minute buckets of one service ending just
// before end, each with one sample of max bytes.
func buckets(service string, end time.Time, n int, max int64, limit *int64) []store.MemoryBucket {
	out := make([]store.MemoryBucket, n)
	for i := range out {
		out[i] = store.MemoryBucket{
			Service:    service,
			Start:      end.Add(-time.Duration(n-i) * store.MemoryBucketSize),
			MaxBytes:   max,
			SumBytes:   max,
			Samples:    1,
			LimitBytes: limit,
		}
	}
	return out
}

func TestSuggestedLimitBytes(t *testing.T) {
	for _, tc := range []struct{ peak, want int64 }{
		{0, 128 * mib},
		{50 * mib, 128 * mib},    // 75m, below the floor
		{100 * mib, 192 * mib},   // 150m → 192m
		{128 * mib, 192 * mib},   // exactly 192m
		{128*mib + 1, 256 * mib}, // just over
		{400 * mib, 640 * mib},   // 600m → 640m
		{1000 * mib, 1536 * mib}, // 1500m → 1536m
	} {
		if got := suggestedLimitBytes(tc.peak); got != tc.want {
			t.Errorf("suggestedLimitBytes(%dm) = %dm, want %dm", tc.peak/mib, got/mib, tc.want/mib)
		}
	}
}

func TestSuggestMemoryLimit(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	limit := i64(512 * mib)
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	t.Run("collecting", func(t *testing.T) {
		s := suggestMemoryLimit(buckets("web", now, 23*12, 200*mib, limit))
		if s.Ready || s.LimitBytes != nil || s.Limit != nil || s.Flag != nil || s.BasisHours != 23 {
			t.Errorf("suggestion = %+v", s)
		}
	})
	t.Run("near the limit even while collecting", func(t *testing.T) {
		s := suggestMemoryLimit(buckets("web", now, 12, 470*mib, limit))
		if s.Ready || str(s.Flag) != "near_limit" {
			t.Errorf("suggestion = %+v flag %s", s, str(s.Flag))
		}
	})
	t.Run("ready", func(t *testing.T) {
		b := buckets("web", now, 30*12, 200*mib, limit)
		b[10].MaxBytes = 400 * mib // one spike: the peak, not the p95
		s := suggestMemoryLimit(b)
		if !s.Ready || s.PeakBytes != 400*mib || s.P95Bytes != 200*mib || *s.LimitBytes != 640*mib || str(s.Limit) != "640m" || s.Flag != nil {
			t.Errorf("suggestion = %+v limit %s flag %s", s, str(s.Limit), str(s.Flag))
		}
		if !s.BasisSince.Equal(b[0].Start) || s.BasisHours != 30 {
			t.Errorf("basis = %v %v", s.BasisSince, s.BasisHours)
		}
	})
	t.Run("generous after a week", func(t *testing.T) {
		if s := suggestMemoryLimit(buckets("web", now, 6*24*12, 100*mib, limit)); s.Flag != nil {
			t.Errorf("flagged before a week: %s", str(s.Flag))
		}
		s := suggestMemoryLimit(buckets("web", now, 7*24*12, 100*mib, limit))
		if str(s.Flag) != "generous" || str(s.Limit) != "192m" {
			t.Errorf("suggestion = %+v flag %s", s, str(s.Flag))
		}
	})
	t.Run("only the current limit counts", func(t *testing.T) {
		old := buckets("web", now.Add(-2*time.Hour), 7*24*12, 500*mib, i64(256*mib))
		current := buckets("web", now, 2*12, 300*mib, i64(1024*mib))
		s := suggestMemoryLimit(append(old, current...))
		if s.Ready || s.PeakBytes != 300*mib || s.BasisHours != 2 || s.Flag != nil {
			t.Errorf("suggestion = %+v flag %s", s, str(s.Flag))
		}
	})
	t.Run("no limit", func(t *testing.T) {
		s := suggestMemoryLimit(buckets("web", now, 8*24*12, 100*mib, nil))
		if s.Flag != nil || !s.Ready {
			t.Errorf("suggestion = %+v", s)
		}
	})
}

func TestMemoryHistoryDownsamples(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	b := buckets("web", now, 12, 100*mib, i64(512*mib)) // 02:00–03:00
	b[3].MaxBytes, b[3].SumBytes, b[3].Samples = 300*mib, 500*mib, 2
	b[11].Restarts = 2

	day := memoryHistory(b, "24h", now)
	if day.StepSeconds != 300 || len(day.Services) != 1 || len(day.Services[0].Points) != 12 {
		t.Fatalf("24h = %+v", day)
	}
	if p := day.Services[0].Points[3]; p.MaxBytes != 300*mib || p.AvgBytes != 250*mib {
		t.Errorf("point = %+v", p)
	}

	week := memoryHistory(b, "7d", now)
	svc := week.Services[0]
	if week.StepSeconds != 1800 || len(svc.Points) != 2 {
		t.Fatalf("7d points = %+v", svc.Points)
	}
	// 02:00–02:30: six buckets, seven samples, one of them 300m at most.
	first := svc.Points[0]
	if !first.At.Equal(now.Add(-time.Hour)) || first.MaxBytes != 300*mib || first.AvgBytes != (5*100+500)*mib/7 || *first.LimitBytes != 512*mib {
		t.Errorf("first point = %+v", first)
	}
	if svc.PeakBytes != 300*mib || svc.Restarts != 2 || *svc.LimitBytes != 512*mib {
		t.Errorf("service = %+v", svc)
	}

	// Buckets older than the range feed the suggestion but are not shown.
	old := buckets("db", now.Add(-48*time.Hour), 12, 100*mib, nil)
	if got := memoryHistory(old, "24h", now); len(got.Services) != 0 {
		t.Errorf("a service with no data in range is listed: %+v", got.Services)
	}
	if got := memoryHistory(append(old, b...), "14d", now); len(got.Services) != 2 || got.Services[0].Name != "db" {
		t.Errorf("14d services = %+v", got.Services)
	}
}

func TestMemoryHistoryAPI(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	app, err := srv.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	now := srv.clock.Now()
	limit := int64(512 * mib)

	rec := request(t, h, http.MethodGet, "/api/apps/blog/memory", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	var empty memoryHistoryJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil || empty.Range != "24h" || empty.Services == nil || len(empty.Services) != 0 {
		t.Errorf("empty = %s", rec.Body)
	}

	for at := now.Add(-26 * time.Hour); at.Before(now); at = at.Add(time.Minute) {
		samples := []store.MemorySample{{AppID: app.ID, Service: "web", Bytes: 200 * mib, LimitBytes: &limit}}
		if at.After(now.Add(-time.Hour)) {
			samples = append(samples, store.MemorySample{AppID: app.ID, Service: "worker", Bytes: 480 * mib, LimitBytes: &limit})
		}
		if err := srv.store.RecordMemorySamples(context.Background(), at, samples); err != nil {
			t.Fatal(err)
		}
	}

	rec = request(t, h, http.MethodGet, "/api/apps/blog/memory?range=7d", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("7d: %d %s", rec.Code, rec.Body)
	}
	var got memoryHistoryJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Range != "7d" || got.StepSeconds != 1800 || len(got.Services) != 2 {
		t.Fatalf("history = %s", rec.Body)
	}
	web, worker := got.Services[0], got.Services[1]
	if web.Name != "web" || !web.Suggestion.Ready || *web.Suggestion.Limit != "320m" || web.Suggestion.Flag != nil {
		t.Errorf("web = %+v", web.Suggestion)
	}
	if worker.Name != "worker" || worker.Suggestion.Ready || worker.Suggestion.Flag == nil || *worker.Suggestion.Flag != "near_limit" {
		t.Errorf("worker = %+v", worker.Suggestion)
	}

	if rec := request(t, h, http.MethodGet, "/api/apps/blog/memory?range=1y", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad range: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/nope/memory", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app: %d", rec.Code)
	}
}
