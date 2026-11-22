package deploy

import (
	"slices"
	"testing"
	"time"
)

func TestUpArgs(t *testing.T) {
	e := &Engine{opts: Options{WaitTimeout: 300 * time.Second}}
	tests := []struct {
		services []string
		want     []string
	}{
		{nil, []string{"up", "-d", "--pull", "missing", "--remove-orphans", "--wait", "--wait-timeout", "300"}},
		{[]string{"web", "worker"}, []string{"up", "-d", "--pull", "missing", "--no-deps", "--wait", "--wait-timeout", "300", "web", "worker"}},
	}
	for _, tt := range tests {
		if got := e.upArgs(tt.services...); !slices.Equal(got, tt.want) {
			t.Errorf("upArgs(%v) = %v, want %v", tt.services, got, tt.want)
		}
	}
}
