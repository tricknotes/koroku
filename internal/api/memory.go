package api

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// Memory history ranges and the step their points are merged into
// (ARCHITECTURE 9.5): about 300 points whatever the range.
var memoryRanges = map[string]struct {
	span, step time.Duration
}{
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"14d": {14 * 24 * time.Hour, time.Hour},
}

// The limit suggestion (ARCHITECTURE 9.5).
const (
	memoryBasisSpan      = 14 * 24 * time.Hour
	memoryReadyBuckets   = 24 * 12     // 24 hours of 5-minute buckets
	memoryWeekBuckets    = 7 * 24 * 12 // 7 days
	memorySuggestStep    = 64 << 20
	memorySuggestMinimum = 128 << 20
)

type memoryPointJSON struct {
	At         time.Time `json:"at"`
	MaxBytes   int64     `json:"max_bytes"`
	AvgBytes   int64     `json:"avg_bytes"`
	LimitBytes *int64    `json:"limit_bytes"`
}

type memorySuggestionJSON struct {
	BasisSince time.Time `json:"basis_since"`
	BasisHours float64   `json:"basis_hours"`
	PeakBytes  int64     `json:"peak_bytes"`
	P95Bytes   int64     `json:"p95_bytes"`
	Ready      bool      `json:"ready"`
	LimitBytes *int64    `json:"limit_bytes"`
	Limit      *string   `json:"limit"`
	// Flag is "near_limit", "generous" or nil.
	Flag *string `json:"flag"`
}

type memoryServiceJSON struct {
	Name       string               `json:"name"`
	LimitBytes *int64               `json:"limit_bytes"`
	PeakBytes  int64                `json:"peak_bytes"`
	Restarts   int                  `json:"restarts"`
	Points     []memoryPointJSON    `json:"points"`
	Suggestion memorySuggestionJSON `json:"suggestion"`
}

type memoryHistoryJSON struct {
	Range       string              `json:"range"`
	StepSeconds int                 `json:"step_seconds"`
	From        time.Time           `json:"from"`
	To          time.Time           `json:"to"`
	Services    []memoryServiceJSON `json:"services"`
}

func (s *Server) handleMemoryHistory(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("range")
	if name == "" {
		name = "24h"
	}
	if _, ok := memoryRanges[name]; !ok {
		writeError(w, http.StatusBadRequest, "invalid_range", "range must be 24h, 7d or 14d")
		return
	}
	now := s.clock.Now()
	buckets, err := s.store.ListMemoryUsage(r.Context(), app.ID, now.Add(-memoryBasisSpan))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, memoryHistory(buckets, name, now))
}

// memoryHistory builds the response from the app's buckets of the last 14
// days, ordered by service and time.
func memoryHistory(buckets []store.MemoryBucket, rangeName string, now time.Time) memoryHistoryJSON {
	rng := memoryRanges[rangeName]
	from := now.Add(-rng.span)
	out := memoryHistoryJSON{
		Range:       rangeName,
		StepSeconds: int(rng.step / time.Second),
		From:        from,
		To:          now,
		Services:    []memoryServiceJSON{},
	}
	for start := 0; start < len(buckets); {
		end := start + 1
		for end < len(buckets) && buckets[end].Service == buckets[start].Service {
			end++
		}
		if svc, ok := memoryService(buckets[start:end], from, rng.step); ok {
			out.Services = append(out.Services, svc)
		}
		start = end
	}
	return out
}

// memoryService summarizes one service's buckets (in time order); ok is
// false when none falls in the displayed range.
func memoryService(buckets []store.MemoryBucket, from time.Time, step time.Duration) (memoryServiceJSON, bool) {
	svc := memoryServiceJSON{Name: buckets[0].Service, Points: []memoryPointJSON{}}
	var sum, samples int64
	for _, b := range buckets {
		if b.Start.Before(from) {
			continue
		}
		at := b.Start.Truncate(step)
		n := len(svc.Points)
		if n == 0 || !svc.Points[n-1].At.Equal(at) {
			if n > 0 && samples > 0 {
				svc.Points[n-1].AvgBytes = sum / samples
			}
			svc.Points = append(svc.Points, memoryPointJSON{At: at})
			sum, samples = 0, 0
			n++
		}
		p := &svc.Points[n-1]
		p.MaxBytes = max(p.MaxBytes, b.MaxBytes)
		if b.LimitBytes != nil && (p.LimitBytes == nil || *b.LimitBytes > *p.LimitBytes) {
			limit := *b.LimitBytes
			p.LimitBytes = &limit
		}
		sum += b.SumBytes
		samples += b.Samples
		svc.PeakBytes = max(svc.PeakBytes, b.MaxBytes)
		svc.Restarts += b.Restarts
	}
	if len(svc.Points) == 0 {
		return memoryServiceJSON{}, false
	}
	if samples > 0 {
		svc.Points[len(svc.Points)-1].AvgBytes = sum / samples
	}
	svc.LimitBytes = buckets[len(buckets)-1].LimitBytes
	svc.Suggestion = suggestMemoryLimit(buckets)
	return svc, true
}

func sameLimit(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// suggestMemoryLimit applies the rule of ARCHITECTURE 9.5 to the buckets
// recorded under the current limit: the trailing run of buckets whose limit
// equals the last one.
func suggestMemoryLimit(buckets []store.MemoryBucket) memorySuggestionJSON {
	current := buckets[len(buckets)-1].LimitBytes
	first := len(buckets) - 1
	for first > 0 && sameLimit(buckets[first-1].LimitBytes, current) {
		first--
	}
	basis := buckets[first:]
	maxima := make([]int64, len(basis))
	for i, b := range basis {
		maxima[i] = b.MaxBytes
	}
	slices.Sort(maxima)
	peak := maxima[len(maxima)-1]
	p95 := maxima[int(math.Ceil(0.95*float64(len(maxima))))-1]

	s := memorySuggestionJSON{
		BasisSince: basis[0].Start,
		BasisHours: math.Round(float64(len(basis))*10/12) / 10,
		PeakBytes:  peak,
		P95Bytes:   p95,
		Ready:      len(basis) >= memoryReadyBuckets,
	}
	if s.Ready {
		limit := suggestedLimitBytes(peak)
		text := strconv.FormatInt(limit>>20, 10) + "m"
		s.LimitBytes, s.Limit = &limit, &text
	}
	if current != nil && *current > 0 {
		var flag string
		switch {
		case peak*10 >= *current*9:
			flag = "near_limit"
		case len(basis) >= memoryWeekBuckets && peak*10 < *current*3:
			flag = "generous"
		}
		if flag != "" {
			s.Flag = &flag
		}
	}
	return s
}

// suggestedLimitBytes is peak × 1.5 rounded up to 64MiB, at least 128MiB.
func suggestedLimitBytes(peak int64) int64 {
	target := (peak*3 + 1) / 2
	limit := (target + memorySuggestStep - 1) / memorySuggestStep * memorySuggestStep
	return max(limit, memorySuggestMinimum)
}
