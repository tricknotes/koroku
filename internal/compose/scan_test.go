package compose

import (
	"os"
	"reflect"
	"testing"
)

func TestScanServices(t *testing.T) {
	const file = `# A typical Rails app
name: blog
services:
  web:
    build: .
    command: ["bin/rails", "server"]   # ports mentioned in a comment: 9999
    ports:
      - "3000:3000"
      - 127.0.0.1:9229:9229/tcp
    environment:
      PORT: "3000"
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17
    ports: ["5432:5432"]
    volumes:
      - pgdata:/var/lib/postgresql/data
  "worker":
    build: .
    command: bin/jobs
  metrics:
    image: example/metrics
    expose:
      - 9100
    ports:
      - target: 8080
        published: "18080"
        protocol: tcp
      - published: 9443
        target: 8443
  cache:
    image: valkey/valkey:8   # no ports at all
    ports:
      - "${CACHE_PORT}:6379"
      - "7000-7002:7000-7002"
      - "${ONLY_VARIABLE}"

volumes:
  pgdata:
    driver: local
`
	want := []ServiceHint{
		{Name: "web", Ports: []int{3000, 9229}},
		{Name: "db", Ports: []int{5432}},
		{Name: "worker"},
		{Name: "metrics", Ports: []int{8080, 8443, 9100}},
		{Name: "cache", Ports: []int{6379, 7000}},
	}
	got := ScanServices([]byte(file))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ScanServices =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestScanServicesVariants(t *testing.T) {
	cases := map[string]struct {
		file string
		want []ServiceHint
	}{
		"four-space indentation with CRLF": {
			"services:\r\n    app:\r\n        image: nginx\r\n        ports:\r\n            - 8080:80\r\n",
			[]ServiceHint{{Name: "app", Ports: []int{80}}},
		},
		"list items at the key's indentation": {
			"services:\n  app:\n    ports:\n    - \"8080:80\"\n    - \"8443:443\"\n    image: nginx\n",
			[]ServiceHint{{Name: "app", Ports: []int{80, 443}}},
		},
		"services is not the first key": {
			"volumes:\n  data:\nservices:\n  api:\n    image: example/api\n    expose: [\"4000\"]\nnetworks:\n  front:\n",
			[]ServiceHint{{Name: "api", Ports: []int{4000}}},
		},
		"no services key":        {"version: \"3\"\nvolumes:\n  data:\n", []ServiceHint{}},
		"flow-style services":    {"services: {web: {image: nginx}}\n", []ServiceHint{}},
		"empty file":             {"", []ServiceHint{}},
		"nested services key":    {"x-templates:\n  services:\n    fake:\n      image: nope\n", []ServiceHint{}},
		"duplicate ports listed": {"services:\n  web:\n    ports:\n      - 80\n      - \"8080:80\"\n", []ServiceHint{{Name: "web", Ports: []int{80}}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ScanServices([]byte(tc.file))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ScanServices = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestScanServicesOnSampleApps(t *testing.T) {
	for file, want := range map[string][]ServiceHint{
		"../../testdata/apps/build-app/compose.yaml": {{Name: "web", Ports: []int{8080}}, {Name: "sidecar", Ports: []int{5432}}},
		"../../testdata/apps/image-app/compose.yaml": {{Name: "web"}},
	} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := ScanServices(data); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ScanServices = %+v, want %+v", file, got, want)
		}
	}
}
