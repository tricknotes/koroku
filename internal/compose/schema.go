package compose

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// The policy check reads the settings it knows how to judge (CheckPolicy).
// Compose has many more, and gains new ones with every release; some of
// them reach the host or another app just as a privileged container does
// (a provider service runs a program on the host, use_api_socket mounts
// the Docker socket, build.ssh hands a host key to the build). So the
// normalized project is also compared with the list below of the settings
// koroku allows, and anything else is a violation (RuleUnsupported): a
// setting koroku has never heard of is refused until someone has looked at
// what it does.
//
// The list names the settings as `docker compose config --format json`
// prints them. A setting that is on the list but dangerous for some values
// (network_mode, security_opt, driver_opts, ...) is judged in CheckPolicy.

// schema describes what may appear at one place of the normalized project.
type schema struct {
	// free allows any value.
	free bool
	// keys are the allowed keys of an object. Keys starting with "x-" are
	// extensions, which Compose ignores, and are always allowed.
	keys map[string]*schema
	// values is the schema of every value of an object whose keys are names
	// (services, volumes).
	values *schema
	// items is the schema of every item of a list.
	items *schema
}

var free = &schema{free: true}

// object is an object with the given keys; names maps a key to its schema
// and the plain names may hold anything.
func object(names map[string]*schema, plain ...string) *schema {
	keys := make(map[string]*schema, len(names)+len(plain))
	for _, name := range plain {
		keys[name] = free
	}
	for name, s := range names {
		keys[name] = s
	}
	return &schema{keys: keys}
}

func mapOf(values *schema) *schema { return &schema{values: values} }
func listOf(items *schema) *schema { return &schema{items: items} }

// hookSchema is a lifecycle hook (post_start, pre_stop). `privileged` is
// left out: it runs the hook with every capability, whatever the service
// itself is allowed.
var hookSchema = listOf(object(nil, "command", "user", "working_dir", "environment"))

var buildSchema = object(nil,
	"context", "dockerfile", "dockerfile_inline", "args", "target", "labels",
	"cache_from", "cache_to", "no_cache", "pull", "shm_size", "platforms",
	"extra_hosts", "network", "privileged", "secrets", "ulimits",
	"additional_contexts", "provenance", "sbom",
	// Not allowed: tags (names the image like another app's image),
	// ssh (hands a host key or agent to the build), entitlements (host
	// networking and more for RUN steps), isolation.
)

var deploySchema = object(map[string]*schema{
	"resources": object(map[string]*schema{
		"limits": object(nil, "cpus", "memory", "pids"),
		// Not allowed: devices and generic_resources (host devices).
		"reservations": object(nil, "cpus", "memory"),
	}),
}, "mode", "replicas", "restart_policy", "update_config", "rollback_config", "labels", "endpoint_mode", "placement")

var mountSchema = object(map[string]*schema{
	// Not allowed: propagation (shared mounts reach back into the host's
	// mount table).
	"bind":   object(nil, "create_host_path", "selinux"),
	"volume": object(nil, "nocopy", "subpath", "labels"),
	"tmpfs":  object(nil, "size", "mode"),
}, "type", "source", "target", "read_only", "consistency")

var serviceSchema = object(map[string]*schema{
	"build":      buildSchema,
	"deploy":     deploySchema,
	"logging":    object(nil, "driver", "options"),
	"post_start": hookSchema,
	"pre_stop":   hookSchema,
	"volumes":    listOf(mountSchema),
},
	"annotations", "attach", "cap_add", "cap_drop", "cgroup", "command", "configs",
	"container_name", "cpu_count", "cpu_percent", "cpu_period", "cpu_quota",
	"cpu_rt_period", "cpu_rt_runtime", "cpu_shares", "cpus", "cpuset", "depends_on",
	"develop", "device_cgroup_rules", "devices", "dns", "dns_opt", "dns_search",
	"domainname", "entrypoint", "env_file", "environment", "expose", "extra_hosts",
	"group_add", "healthcheck", "hostname", "image", "init", "ipc", "labels", "links",
	"mac_address", "mem_limit", "mem_reservation", "mem_swappiness", "memswap_limit",
	"network_mode", "networks", "oom_kill_disable", "oom_score_adj", "pid",
	"pids_limit", "platform", "ports", "privileged", "profiles", "pull_policy",
	"pull_refresh_after", "read_only", "restart", "runtime", "scale", "secrets",
	"security_opt", "shm_size", "stdin_open", "stop_grace_period", "stop_signal",
	"storage_opt", "sysctls", "tmpfs", "tty", "ulimits", "user", "userns_mode",
	"uts", "volumes_from", "working_dir",
	// Not allowed, among others: provider (runs a program on the host),
	// use_api_socket (mounts the Docker socket and the registry logins),
	// gpus, models, cgroup_parent, external_links, label_file,
	// credential_spec, blkio_config, isolation.
)

var projectSchema = object(map[string]*schema{
	"services": mapOf(serviceSchema),
	// Not allowed: driver_opts (a bridge can be told to be another
	// network's bridge).
	"networks": mapOf(object(nil, "name", "driver", "external", "internal", "attachable",
		"enable_ipv4", "enable_ipv6", "ipam", "labels")),
	"volumes": mapOf(object(nil, "name", "driver", "driver_opts", "external", "labels")),
	// Not allowed: driver, template_driver.
	"secrets": mapOf(object(nil, "name", "file", "environment", "content", "external", "labels")),
	"configs": mapOf(object(nil, "name", "file", "environment", "content", "external", "labels")),
	// Not allowed: models, and include (resolved before the project is
	// normalized; see ScanFiles).
}, "name", "version")

// unsupported is one setting of the normalized project that is not on the
// list.
type unsupported struct {
	// Service is the service the setting belongs to; empty for the others.
	Service string
	// Path names the setting from its service or from the top of the file,
	// such as "build.ssh" or "networks.backend.driver_opts".
	Path string
}

// unsupportedSettings lists, sorted, the settings of the normalized project
// that koroku does not allow. skip names services that are not looked at.
// A setting with an empty value (null, false, 0, "", [] or {}) says nothing
// and is not reported.
func unsupportedSettings(raw json.RawMessage, skip func(service string) bool) []unsupported {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil // ParseProject has refused the input already
	}
	var found []unsupported
	walkSchema(doc, projectSchema, nil, skip, &found)
	sort.Slice(found, func(i, j int) bool {
		if found[i].Service != found[j].Service {
			return found[i].Service < found[j].Service
		}
		return found[i].Path < found[j].Path
	})
	// A list reports the same setting once per item.
	out := found[:0]
	for i, f := range found {
		if i == 0 || f != found[i-1] {
			out = append(out, f)
		}
	}
	return out
}

func walkSchema(value any, s *schema, path []string, skip func(string) bool, found *[]unsupported) {
	if s.free || emptyValue(value) {
		return
	}
	report := func(path []string) {
		u := unsupported{Path: strings.Join(path, ".")}
		if len(path) >= 2 && path[0] == "services" {
			u.Service, u.Path = path[1], strings.Join(path[2:], ".")
		}
		*found = append(*found, u)
	}
	switch v := value.(type) {
	case map[string]any:
		switch {
		case s.values != nil:
			for name, item := range v {
				if len(path) == 1 && path[0] == "services" && skip != nil && skip(name) {
					continue
				}
				walkSchema(item, s.values, append(path, name), skip, found)
			}
		case s.keys != nil:
			for key, item := range v {
				if strings.HasPrefix(key, "x-") {
					continue
				}
				child, ok := s.keys[key]
				if !ok {
					if !emptyValue(item) {
						report(append(path, key))
					}
					continue
				}
				walkSchema(item, child, append(path, key), skip, found)
			}
		default:
			report(path)
		}
	case []any:
		if s.items == nil {
			report(path)
			return
		}
		for _, item := range v {
			walkSchema(item, s.items, path, skip, found)
		}
	default:
		// A scalar where an object or a list is expected: a form of the
		// setting koroku does not know.
		report(path)
	}
}

func emptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case bool:
		return !v
	case string:
		return v == ""
	case json.Number:
		return v.String() == "0"
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	}
	return false
}
