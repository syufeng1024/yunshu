// Package metrics implements the TDS §5.3 metric surface without a
// third-party client library.
//
// Hot path: a series is resolved by With, which is lock-free on a hit (one
// atomic pointer load and one map lookup over a stack-built key). The
// recording call is a single atomic add for a counter; a duration histogram
// also adds one bucket counter because a sum cannot be derived from the
// buckets. There is no mutex and no per-request map allocation on the hit path.
//
// Export has two shapes on purpose:
//
//   - Snapshot/PrometheusText return cumulative values for GET /metrics. A
//     Prometheus counter must be cumulative or rate() sees a reset every
//     scrape.
//   - Run(interval, sink) emits counter deltas for the window. A downstream
//     accumulator that receives cumulative values double-counts every window
//     (TDS §5.3).
package metrics

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Kind is the Prometheus metric type.
type Kind uint8

// Metric kinds.
const (
	KindCounter Kind = iota + 1
	KindGauge
	KindHistogram
)

// String returns the text-format type name.
func (k Kind) String() string {
	switch k {
	case KindCounter:
		return "counter"
	case KindGauge:
		return "gauge"
	case KindHistogram:
		return "histogram"
	default:
		return "untyped"
	}
}

const (
	// DefaultMaxSeries is the per-registry series ceiling (TDS §5.4). Reaching
	// it rejects new label combinations; existing series are never evicted,
	// because an evicted series disappears from the scrape and Prometheus
	// marks it stale.
	DefaultMaxSeries = 10_000
	// DefaultWindow is the Registry.Run aggregation interval.
	DefaultWindow = 10 * time.Second

	maxLabelValueLen = 128
	maxKeyLen        = 512
)

// DurationBuckets are the fixed histogram bounds from TDS §5.4. 15s and 120s
// are deliberately present: they are the P99 alert thresholds for TTFT and
// non-stream request duration, so a bucket edge sits exactly on each threshold.
//
// 540 and 600 were added for the streaming half of the latency alert (G27):
// histogram_quantile returns the highest finite bucket bound when the quantile
// falls in the +Inf bucket, so a threshold above the last edge can never be
// reached — a 540s stream alert against a 300s ceiling would be a rule that is
// false forever, which is worse than no rule. 540s = 90% of the 600s stream
// budget (gw.DefaultStreamBudgetMS); 600 is its hard edge.
var DurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15, 30, 60, 120, 300,
	540, 600,
}

// Label is one name/value pair in export order.
type Label struct {
	Name  string
	Value string
}

// Sample is one series value. Counter and gauge samples use Value; histogram
// samples use Buckets (per-bucket, non-cumulative), Sum (seconds) and Count.
type Sample struct {
	Labels  []Label
	Value   int64
	Buckets []uint64
	Sum     float64
	Count   uint64
}

// FamilySnapshot is one metric family. A family with no Samples is still
// rendered with HELP and TYPE: that is how a lazily-created metric (no fake
// zero at startup) stays visible to the A8 name check without lying about a
// value.
type FamilySnapshot struct {
	Name    string
	Help    string
	Kind    Kind
	Buckets []float64
	Samples []Sample
}

// Snapshot is a point-in-time view of a Registry.
type Snapshot struct {
	Families []FamilySnapshot
}

// Sink receives one window snapshot from Registry.Run. Counters carry the
// delta since the previous call.
type Sink func(Snapshot)

// Registry owns metric families and their label combinations.
type Registry struct {
	mu        sync.Mutex
	families  []*family
	byName    map[string]*family
	maxSeries int

	seriesCount    atomic.Int64
	seriesRejected atomic.Int64
	invalidLabels  atomic.Int64

	lastSeriesRejected atomic.Int64
	lastInvalidLabels  atomic.Int64
}

// NewRegistry returns a registry with the 10,000-series ceiling.
func NewRegistry() *Registry { return NewRegistryWithLimit(DefaultMaxSeries) }

// NewRegistryWithLimit returns a registry with an explicit ceiling. Tests use
// a small value; production uses NewRegistry.
func NewRegistryWithLimit(max int) *Registry {
	if max <= 0 {
		max = DefaultMaxSeries
	}
	return &Registry{byName: make(map[string]*family), maxSeries: max}
}

// Counter registers a counter family.
func (r *Registry) Counter(name, help string, labelNames ...string) *CounterVec {
	return &CounterVec{f: newFamily(r, KindCounter, name, help, labelNames, nil)}
}

// Gauge registers a gauge family.
func (r *Registry) Gauge(name, help string, labelNames ...string) *GaugeVec {
	return &GaugeVec{f: newFamily(r, KindGauge, name, help, labelNames, nil)}
}

// Histogram registers a fixed-bucket duration histogram family. Buckets are
// upper bounds in seconds.
func (r *Registry) Histogram(name, help string, buckets []float64, labelNames ...string) *HistogramVec {
	return &HistogramVec{f: newFamily(r, KindHistogram, name, help, labelNames, buckets)}
}

// SeriesCount reports how many label combinations exist.
func (r *Registry) SeriesCount() int64 {
	if r == nil {
		return 0
	}
	return r.seriesCount.Load()
}

// SeriesRejected reports how many new combinations were refused at the
// ceiling.
func (r *Registry) SeriesRejected() int64 {
	if r == nil {
		return 0
	}
	return r.seriesRejected.Load()
}

func (r *Registry) register(f *family) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch f.name {
	case "metrics_series_rejected_total", "metrics_invalid_labels_total":
		panic("metrics: " + f.name + " is reserved")
	}
	if _, exists := r.byName[f.name]; exists {
		panic("metrics: duplicate metric " + f.name)
	}
	r.byName[f.name] = f
	r.families = append(r.families, f)
}

type family struct {
	reg        *Registry
	name       string
	help       string
	kind       Kind
	labelNames []string
	buckets    []float64

	mu   sync.Mutex
	fast atomic.Pointer[map[string]*child]
	noop *child
}

func newFamily(r *Registry, kind Kind, name, help string, labelNames []string, buckets []float64) *family {
	f := &family{
		reg:        r,
		name:       name,
		help:       help,
		kind:       kind,
		labelNames: append([]string(nil), labelNames...),
	}
	if kind == KindHistogram {
		f.buckets = append([]float64(nil), buckets...)
	}
	f.noop = newChild(f, nil)
	f.noop.noop = true
	r.register(f)
	return f
}

// with resolves one label combination. On a hit it does not take a lock: the
// child map is immutable after publication and read through an atomic pointer.
func (f *family) with(values ...string) *child {
	if f == nil {
		return nil
	}
	if len(values) != len(f.labelNames) {
		f.reg.invalidLabels.Add(1)
		return f.noop
	}
	var stack [maxKeyLen]byte
	key := appendKey(stack[:0], values)
	if m := f.fast.Load(); m != nil {
		if c := (*m)[string(key)]; c != nil {
			return c
		}
	}
	return f.create(string(key), values)
}

func (f *family) create(key string, values []string) *child {
	r := f.reg
	r.mu.Lock()
	defer r.mu.Unlock()

	if m := f.fast.Load(); m != nil {
		if c := (*m)[key]; c != nil {
			return c
		}
	}
	if r.seriesCount.Load() >= int64(r.maxSeries) {
		r.seriesRejected.Add(1)
		return f.noop
	}

	stored := make([]string, len(values))
	for i := range values {
		stored[i] = sanitizeLabelValue(values[i])
	}
	c := newChild(f, stored)

	old := f.fast.Load()
	var m map[string]*child
	if old != nil {
		m = make(map[string]*child, len(*old)+1)
		for k, v := range *old {
			m[k] = v
		}
	} else {
		m = make(map[string]*child, 4)
	}
	m[key] = c
	f.fast.Store(&m)
	r.seriesCount.Add(1)
	return c
}

type child struct {
	family      *family
	labelValues []string
	noop        bool

	value      atomic.Int64
	lastExport atomic.Int64

	bucketCounts []atomic.Uint64
	sumNS        atomic.Int64

	counter   *Counter
	gauge     *Gauge
	histogram *Histogram
}

func newChild(f *family, values []string) *child {
	c := &child{family: f, labelValues: values}
	switch f.kind {
	case KindCounter:
		c.counter = &Counter{c: c}
	case KindGauge:
		c.gauge = &Gauge{c: c}
	case KindHistogram:
		c.bucketCounts = make([]atomic.Uint64, len(f.buckets)+1)
		c.histogram = &Histogram{c: c}
	}
	return c
}

func (c *child) add(delta int64) {
	if c == nil || c.noop {
		return
	}
	if delta < 0 {
		// Counters are monotonic. A negative delta is a caller bug; silently
		// ignoring it is safer than exporting a negative rate.
		return
	}
	c.value.Add(delta)
}

func (c *child) set(v int64) {
	if c == nil || c.noop {
		return
	}
	c.value.Store(v)
}

func (c *child) observe(d time.Duration) {
	if c == nil || c.noop {
		return
	}
	if d < 0 {
		d = 0
	}
	seconds := d.Seconds()
	idx := len(c.family.buckets) // +Inf
	for i, bound := range c.family.buckets {
		if seconds <= bound {
			idx = i
			break
		}
	}
	c.bucketCounts[idx].Add(1)
	c.sumNS.Add(int64(d))
}

// Counter is one counter series.
type Counter struct{ c *child }

// CounterVec is a counter family.
type CounterVec struct{ f *family }

var emptyCounter = &Counter{c: &child{noop: true}}

// With resolves one label combination. The returned handle is stable and
// allocation-free after the first call for that combination.
func (v *CounterVec) With(values ...string) *Counter {
	if v == nil || v.f == nil {
		return emptyCounter
	}
	c := v.f.with(values...)
	if c == nil || c.counter == nil {
		return emptyCounter
	}
	return c.counter
}

// Add increments the counter. Negative deltas are ignored.
func (c *Counter) Add(delta int64) {
	if c == nil || c.c == nil {
		return
	}
	c.c.add(delta)
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Value returns the cumulative value.
func (c *Counter) Value() int64 {
	if c == nil || c.c == nil {
		return 0
	}
	return c.c.value.Load()
}

// Gauge is one gauge series.
type Gauge struct{ c *child }

// GaugeVec is a gauge family.
type GaugeVec struct{ f *family }

var emptyGauge = &Gauge{c: &child{noop: true}}

// With resolves one label combination.
func (v *GaugeVec) With(values ...string) *Gauge {
	if v == nil || v.f == nil {
		return emptyGauge
	}
	c := v.f.with(values...)
	if c == nil || c.gauge == nil {
		return emptyGauge
	}
	return c.gauge
}

// Set stores v.
func (g *Gauge) Set(v int64) {
	if g == nil || g.c == nil {
		return
	}
	g.c.set(v)
}

// Add adds delta to the gauge.
func (g *Gauge) Add(delta int64) {
	if g == nil || g.c == nil || g.c.noop {
		return
	}
	g.c.value.Add(delta)
}

// Value returns the current value.
func (g *Gauge) Value() int64 {
	if g == nil || g.c == nil {
		return 0
	}
	return g.c.value.Load()
}

// Histogram is one histogram series.
type Histogram struct{ c *child }

// HistogramVec is a histogram family.
type HistogramVec struct{ f *family }

var emptyHistogram = &Histogram{c: &child{noop: true}}

// With resolves one label combination.
func (v *HistogramVec) With(values ...string) *Histogram {
	if v == nil || v.f == nil {
		return emptyHistogram
	}
	c := v.f.with(values...)
	if c == nil || c.histogram == nil {
		return emptyHistogram
	}
	return c.histogram
}

// ObserveDuration records one duration in the fixed buckets. The sum is stored
// as integer nanoseconds, which keeps the hot path to one atomic add for the
// sum plus one for the bucket; there is no CAS loop.
func (h *Histogram) ObserveDuration(d time.Duration) {
	if h == nil || h.c == nil {
		return
	}
	h.c.observe(d)
}

// Snapshot returns cumulative values. GET /metrics uses this shape.
func (r *Registry) Snapshot() Snapshot {
	return r.snapshot(false)
}

// windowSnapshot returns counter deltas. Only Registry.Run calls it.
func (r *Registry) windowSnapshot() Snapshot {
	return r.snapshot(true)
}

func (r *Registry) snapshot(window bool) Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	families := append([]*family(nil), r.families...)
	r.mu.Unlock()

	out := Snapshot{Families: make([]FamilySnapshot, 0, len(families)+2)}
	for _, f := range families {
		out.Families = append(out.Families, f.snapshot(window))
	}
	out.Families = append(out.Families,
		counterSnapshot("metrics_series_rejected_total",
			"New series rejected after the registry ceiling was reached.",
			&r.seriesRejected, &r.lastSeriesRejected, window),
		counterSnapshot("metrics_invalid_labels_total",
			"Record calls rejected because the label count did not match the family.",
			&r.invalidLabels, &r.lastInvalidLabels, window),
	)
	sort.Slice(out.Families, func(i, j int) bool {
		return out.Families[i].Name < out.Families[j].Name
	})
	return out
}

func (f *family) snapshot(window bool) FamilySnapshot {
	fs := FamilySnapshot{Name: f.name, Help: f.help, Kind: f.kind}
	if len(f.buckets) > 0 {
		fs.Buckets = append([]float64(nil), f.buckets...)
	}
	m := f.fast.Load()
	if m == nil {
		return fs
	}
	keys := make([]string, 0, len(*m))
	for k := range *m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fs.Samples = make([]Sample, 0, len(keys))
	for _, k := range keys {
		c := (*m)[k]
		if c == nil || c.noop {
			continue
		}
		fs.Samples = append(fs.Samples, c.sample(window))
	}
	return fs
}

func (c *child) sample(window bool) Sample {
	f := c.family
	s := Sample{Labels: make([]Label, len(f.labelNames))}
	for i, name := range f.labelNames {
		v := ""
		if i < len(c.labelValues) {
			v = c.labelValues[i]
		}
		s.Labels[i] = Label{Name: name, Value: v}
	}
	switch f.kind {
	case KindCounter:
		total := c.value.Load()
		if window {
			last := c.lastExport.Swap(total)
			s.Value = total - last
			if s.Value < 0 {
				s.Value = 0
			}
		} else {
			s.Value = total
		}
	case KindGauge:
		s.Value = c.value.Load()
	case KindHistogram:
		s.Buckets = make([]uint64, len(c.bucketCounts))
		var count uint64
		for i := range c.bucketCounts {
			s.Buckets[i] = c.bucketCounts[i].Load()
			count += s.Buckets[i]
		}
		s.Count = count
		s.Sum = float64(c.sumNS.Load()) / float64(time.Second)
	}
	return s
}

// appendKey encodes label values without a map or a per-call allocation on
// the read path: the four-byte length prefix makes the key unambiguous, so a
// value containing the separator byte cannot collide with another tuple.
func appendKey(dst []byte, values []string) []byte {
	for _, v := range values {
		v = sanitizeLabelValue(v)
		n := len(v)
		dst = append(dst, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
		dst = append(dst, v...)
	}
	return dst
}

// sanitizeLabelValue strips control characters (a newline would forge a text
// line) and caps the value length. Values from a bounded set are returned
// unchanged, without allocating.
func sanitizeLabelValue(s string) string {
	if s == "" {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) > maxLabelValueLen {
		for len(s) > maxLabelValueLen {
			_, size := utf8.DecodeLastRuneInString(s)
			s = s[:len(s)-size]
		}
	}
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r < 0x20 || r == 0x7f:
			// Drop other control characters.
		default:
			b.WriteRune(r)
		}
		if b.Len() >= maxLabelValueLen {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func counterSnapshot(name, help string, current, last *atomic.Int64, window bool) FamilySnapshot {
	fs := FamilySnapshot{Name: name, Help: help, Kind: KindCounter}
	v := current.Load()
	if window {
		v -= last.Swap(v)
		if v < 0 {
			v = 0
		}
	}
	fs.Samples = []Sample{{Value: v}}
	return fs
}

// PrometheusText renders the cumulative snapshot in the Prometheus text
// exposition format (version 0.0.4).
func (r *Registry) PrometheusText() []byte {
	return EncodeText(r.Snapshot())
}

// EncodeText renders a snapshot in the Prometheus text exposition format.
func EncodeText(s Snapshot) []byte {
	var b bytes.Buffer
	for _, f := range s.Families {
		b.WriteString("# HELP ")
		b.WriteString(f.Name)
		b.WriteByte(' ')
		b.WriteString(escapeHelp(f.Help))
		b.WriteByte('\n')
		b.WriteString("# TYPE ")
		b.WriteString(f.Name)
		b.WriteByte(' ')
		b.WriteString(f.Kind.String())
		b.WriteByte('\n')
		for _, sample := range f.Samples {
			if f.Kind == KindHistogram {
				writeHistogram(&b, f, sample)
				continue
			}
			writeIntSample(&b, f.Name, sample.Labels, sample.Value)
		}
	}
	return b.Bytes()
}

func writeIntSample(b *bytes.Buffer, name string, labels []Label, value int64) {
	b.WriteString(name)
	writeLabels(b, labels, "")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatInt(value, 10))
	b.WriteByte('\n')
}

func writeHistogram(b *bytes.Buffer, f FamilySnapshot, s Sample) {
	var cumulative uint64
	for i, bound := range f.Buckets {
		if i < len(s.Buckets) {
			cumulative += s.Buckets[i]
		}
		b.WriteString(f.Name)
		b.WriteString("_bucket")
		writeLabels(b, s.Labels, strconv.FormatFloat(bound, 'g', -1, 64))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatUint(cumulative, 10))
		b.WriteByte('\n')
	}
	b.WriteString(f.Name)
	b.WriteString("_bucket")
	writeLabels(b, s.Labels, "+Inf")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(s.Count, 10))
	b.WriteByte('\n')

	b.WriteString(f.Name)
	b.WriteString("_sum")
	writeLabels(b, s.Labels, "")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(s.Sum, 'g', -1, 64))
	b.WriteByte('\n')

	b.WriteString(f.Name)
	b.WriteString("_count")
	writeLabels(b, s.Labels, "")
	b.WriteByte(' ')
	b.WriteString(strconv.FormatUint(s.Count, 10))
	b.WriteByte('\n')
}

func writeLabels(b *bytes.Buffer, labels []Label, le string) {
	if len(labels) == 0 && le == "" {
		return
	}
	b.WriteByte('{')
	first := true
	for _, l := range labels {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(l.Name)
		b.WriteString(`="`)
		writeEscaped(b, l.Value)
		b.WriteByte('"')
	}
	if le != "" {
		if !first {
			b.WriteByte(',')
		}
		b.WriteString(`le="`)
		b.WriteString(le)
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

func escapeHelp(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeEscaped(b *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
}

// Run aggregates a window every interval and hands it to sink. Counters carry
// the delta since the previous window; gauges carry their current value.
// A nil sink or a nil context is a no-op, so a caller without a downstream
// consumer does not start a useless goroutine.
func (r *Registry) Run(ctx context.Context, interval time.Duration, sink Sink) error {
	if r == nil || sink == nil || ctx == nil {
		return nil
	}
	if interval <= 0 {
		interval = DefaultWindow
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			sink(r.windowSnapshot())
			return nil
		case <-ticker.C:
			sink(r.windowSnapshot())
		}
	}
}
