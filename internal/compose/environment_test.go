package compose

import (
	"reflect"
	"testing"
)

func TestScanEnvironment(t *testing.T) {
	const file = `services:
  web:
    build: .
    environment:
      RAILS_ENV: production        # fixed by the file
      RACK_ENV: ${RACK_ENV:-production}
      "QUOTED": "yes"
      PASS_THROUGH:
      NULLED: null
    ports:
      - "3000:3000"
  worker:
    image: example/worker
    environment:
    - NODE_ENV=production
    - "DEBUG=0"
    - FROM_HOST
    - INTERPOLATED=$HOME
    command: run
  db:
    image: postgres:17
    environment: [POSTGRES_DB=app]
    healthcheck:
      test: ["CMD", "pg_isready"]
      environment:
        NOT_A_SERVICE_KEY: x
volumes:
  data:
    environment:
      NOT_IN_SERVICES: x
`
	got := ScanEnvironment([]byte(file))
	want := []EnvironmentHint{
		{Service: "web", Key: "RAILS_ENV", Literal: true},
		{Service: "web", Key: "RACK_ENV", Literal: false},
		{Service: "web", Key: "QUOTED", Literal: true},
		{Service: "web", Key: "PASS_THROUGH", Literal: false},
		{Service: "web", Key: "NULLED", Literal: false},
		{Service: "worker", Key: "NODE_ENV", Literal: true},
		{Service: "worker", Key: "DEBUG", Literal: true},
		{Service: "worker", Key: "FROM_HOST", Literal: false},
		{Service: "worker", Key: "INTERPOLATED", Literal: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScanEnvironment:\n got %+v\nwant %+v", got, want)
	}
}

func TestScanEnvironmentNothing(t *testing.T) {
	for name, file := range map[string]string{
		"empty":       "",
		"no services": "name: x\n",
		"flow map":    "services:\n  web:\n    environment: {A: b}\n",
	} {
		if got := ScanEnvironment([]byte(file)); len(got) != 0 {
			t.Errorf("%s: got %+v, want none", name, got)
		}
	}
}
