// Package metrics exposes Prometheus text-format metrics without pulling in
// a client library: request counters plus gauges computed at scrape time.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry holds counters and scrape-time collectors.
type Registry struct {
	mu         sync.Mutex
	counters   map[string]map[string]float64 // name -> labels -> value
	help       map[string]string
	collectors []func(emit func(name, help string, labels map[string]string, value float64))
	start      time.Time
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{counters: map[string]map[string]float64{}, help: map[string]string{}, start: time.Now()}
}

// Inc adds 1 to a counter.
func (r *Registry) Inc(name, help string, labels map[string]string) {
	r.Add(name, help, labels, 1)
}

// Add adds v to a counter.
func (r *Registry) Add(name, help string, labels map[string]string, v float64) {
	key := encodeLabels(labels)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counters[name] == nil {
		r.counters[name] = map[string]float64{}
		r.help[name] = help
	}
	r.counters[name][key] += v
}

// Collect registers a function that emits gauges on every scrape.
func (r *Registry) Collect(fn func(emit func(name, help string, labels map[string]string, value float64))) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, fn)
}

// Write renders all metrics.
func (r *Registry) Write(w io.Writer) error {
	type sample struct {
		labels string
		value  float64
	}
	gauges := map[string][]sample{}
	gaugeHelp := map[string]string{}

	r.mu.Lock()
	collectors := append([]func(func(string, string, map[string]string, float64)){}, r.collectors...)
	counters := map[string][]sample{}
	for name, vals := range r.counters {
		for l, v := range vals {
			counters[name] = append(counters[name], sample{l, v})
		}
	}
	help := map[string]string{}
	for k, v := range r.help {
		help[k] = v
	}
	r.mu.Unlock()

	gauges["type3_uptime_seconds"] = []sample{{"", time.Since(r.start).Seconds()}}
	gaugeHelp["type3_uptime_seconds"] = "Seconds since the Type3 process started."
	for _, c := range collectors {
		c(func(name, h string, labels map[string]string, v float64) {
			gauges[name] = append(gauges[name], sample{encodeLabels(labels), v})
			gaugeHelp[name] = h
		})
	}

	var b strings.Builder
	writeFamily := func(name, typ, h string, ss []sample) {
		sort.Slice(ss, func(i, j int) bool { return ss[i].labels < ss[j].labels })
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, h, name, typ)
		for _, s := range ss {
			fmt.Fprintf(&b, "%s%s %g\n", name, s.labels, s.value)
		}
	}
	for _, name := range sortedKeys(counters) {
		writeFamily(name, "counter", help[name], counters[name])
	}
	for _, name := range sortedKeys(gauges) {
		writeFamily(name, "gauge", gaugeHelp[name], gauges[name])
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func encodeLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := sortedKeys(labels)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(labels[k])
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
