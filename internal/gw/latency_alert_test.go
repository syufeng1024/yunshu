package gw

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	obsmetrics "yunshu.dev/core/internal/obs/metrics"
)

// alertExpr returns the (whitespace-collapsed) expr of one rule in
// internal/obs/alert/rules.yaml. Textual on purpose: the file is hand-written
// YAML and this package has no YAML dependency.
func alertExpr(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "obs", "alert", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	start := strings.Index(string(raw), "- alert: "+name)
	if start < 0 {
		t.Fatalf("rule %s not found in rules.yaml", name)
	}
	body := string(raw)[start:]
	if next := strings.Index(body[1:], "\n      - alert: "); next >= 0 {
		body = body[:next+1]
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "expr:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "expr:"))
		if value == ">-" || value == "|" || value == ">" {
			value = ""
			for _, cont := range lines[i+1:] {
				indent := len(cont) - len(strings.TrimLeft(cont, " "))
				if strings.TrimSpace(cont) == "" || indent <= 8 {
					break
				}
				value += " " + strings.TrimSpace(cont)
			}
		}
		return strings.Join(strings.Fields(value), " ")
	}
	t.Fatalf("rule %s has no expr", name)
	return ""
}

// TestLatencyAlertsAreSplitByStream pins G27: the two latency rules must read
// different halves of the same histogram.
//
// The mount is one metric with a stream label, and the two budgets differ by a
// factor of five (120s unary, 600s stream). Without the label selector a single
// healthy long stream pushes the shared P99 past 120s and pages someone for a
// request that was never going to time out.
func TestLatencyAlertsAreSplitByStream(t *testing.T) {
	unarySeconds := int64(DefaultClientBudgetMS) / 1000
	streamSeconds := int64(DefaultStreamBudgetMS) / 1000
	streamWarn := streamSeconds * 9 / 10

	unary := alertExpr(t, "GatewayLatencyP99High")
	if !strings.Contains(unary, `stream="false"`) {
		t.Fatalf("GatewayLatencyP99High does not filter stream=\"false\": %q", unary)
	}
	if !strings.Contains(unary, "> "+strconv.FormatInt(unarySeconds, 10)) {
		t.Fatalf("GatewayLatencyP99High threshold is not the %ds unary budget (DefaultClientBudgetMS): %q",
			unarySeconds, unary)
	}

	stream := alertExpr(t, "GatewayStreamLatencyP99High")
	if !strings.Contains(stream, `stream="true"`) {
		t.Fatalf("GatewayStreamLatencyP99High does not filter stream=\"true\": %q", stream)
	}
	if !strings.Contains(stream, "> "+strconv.FormatInt(streamWarn, 10)) {
		t.Fatalf("GatewayStreamLatencyP99High threshold is not %ds (90%% of the %ds stream budget): %q",
			streamWarn, streamSeconds, stream)
	}
	if streamWarn == unarySeconds {
		t.Fatalf("the stream threshold equals the unary one; G27 exists because they differ")
	}
}

// TestDurationBucketsReachTheStreamThreshold is the "rule that can never fire"
// guard: histogram_quantile returns the highest finite bound when the quantile
// lands in the +Inf bucket, so a threshold above the last edge is silently
// unreachable.
func TestDurationBucketsReachTheStreamThreshold(t *testing.T) {
	streamWarn := float64(int64(DefaultStreamBudgetMS)) / 1000 * 0.9
	var hasWarn, hasMax bool
	for _, b := range obsmetrics.DurationBuckets {
		if b == streamWarn {
			hasWarn = true
		}
		if b == float64(int64(DefaultStreamBudgetMS))/1000 {
			hasMax = true
		}
	}
	if !hasWarn {
		t.Fatalf("DurationBuckets has no edge at %.0fs, so GatewayStreamLatencyP99High can never "+
			"exceed it: %v", streamWarn, obsmetrics.DurationBuckets)
	}
	if !hasMax {
		t.Fatalf("DurationBuckets has no edge at the stream budget %ds: %v",
			DefaultStreamBudgetMS/1000, obsmetrics.DurationBuckets)
	}
	if last := obsmetrics.DurationBuckets[len(obsmetrics.DurationBuckets)-1]; last <= streamWarn {
		t.Fatalf("last bucket edge %.1fs must sit above the %.0fs stream warning threshold", last, streamWarn)
	}
	// The unary threshold must keep its exact edge: TDS §5.4 chose the bounds so
	// "over the threshold" never depends on bucket interpolation.
	var hasUnary bool
	for _, b := range obsmetrics.DurationBuckets {
		if b == float64(int64(DefaultClientBudgetMS))/1000 {
			hasUnary = true
		}
	}
	if !hasUnary {
		t.Fatalf("DurationBuckets lost the %ds unary edge: %v", DefaultClientBudgetMS/1000, obsmetrics.DurationBuckets)
	}
}
