package compose

import (
	"reflect"
	"testing"
)

func TestDependencies(t *testing.T) {
	// The long form `docker compose config` prints.
	p, err := ParseProject([]byte(`{"services": {
		"web":    {"image": "app", "depends_on": {"db": {"condition": "service_healthy", "required": true}, "cache": {"condition": "service_started"}}},
		"worker": {"image": "app", "depends_on": {"web": {"condition": "service_started"}}},
		"db":     {"image": "postgres", "depends_on": {"volume-init": {"condition": "service_completed_successfully"}}},
		"cache":  {"image": "redis"},
		"volume-init": {"image": "busybox"},
		"loop-a": {"image": "busybox", "depends_on": {"loop-b": {}}},
		"loop-b": {"image": "busybox", "depends_on": {"loop-a": {}}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		service string
		want    []string
	}{
		{"web", []string{"cache", "db", "volume-init"}},
		{"worker", []string{"cache", "db", "volume-init", "web"}},
		{"cache", nil},
		{"missing", nil},
		{"loop-a", []string{"loop-b"}},
	}
	for _, tt := range tests {
		if got := p.Dependencies(tt.service); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Dependencies(%s) = %v, want %v", tt.service, got, tt.want)
		}
	}
}
