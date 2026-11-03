package apierr

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestPlatformAvailabilityWhitelist pins PRD §9.3's eight numerator codes. The
// list is an external promise (it defines a published availability number), so
// growing it must be a deliberate act with a failing test in the way (G26).
func TestPlatformAvailabilityWhitelist(t *testing.T) {
	codes := PlatformAvailabilityCodes()
	if len(codes) != 8 {
		t.Fatalf("whitelist has %d codes; PRD §9.3 fixes it at 8: %v", len(codes), codes)
	}

	seen := map[string]bool{}
	for _, code := range codes {
		if seen[code] {
			t.Fatalf("duplicate %q in the whitelist", code)
		}
		seen[code] = true
		spec, ok := Lookup(code)
		if !ok {
			t.Fatalf("whitelist code %q is not in the spec table (typo?)", code)
		}
		if spec.Owner != OwnerPlatform {
			t.Fatalf("whitelist code %q has Owner=%s; every §9.3 numerator code must be a "+
				"platform-owned failure", code, spec.Owner)
		}
	}

	// The four OwnerPlatform codes that are deliberately NOT in the numerator.
	// If one of these ever becomes "platform availability", the number changes
	// meaning: upstream credential faults would be reported as our downtime.
	for _, code := range []string{
		CodePriceNotConfigured,
		CodeUpstreamAuthFailed,
		CodeUpstreamModelNotFound,
		CodeUpstreamKeyUnavailable,
	} {
		if IsPlatformAvailabilityCode(code) {
			t.Fatalf("%s must NOT count as platform unavailability (PRD §9.3 keeps upstream "+
				"failures in 上游依赖可用性)", code)
		}
		spec, ok := Lookup(code)
		if !ok {
			t.Fatalf("%s missing from the spec table", code)
		}
		if spec.Owner != OwnerPlatform {
			t.Fatalf("%s is Owner=%s; this test documents the Owner-vs-whitelist split and "+
				"must be updated deliberately if Owner changed", code, spec.Owner)
		}
	}

	// OwnerPlatform is strictly larger than the whitelist: that is the whole
	// reason IsPlatformAvailabilityCode exists instead of an Owner comparison.
	ownerPlatform := 0
	for _, spec := range Specs() {
		if spec.Owner == OwnerPlatform {
			ownerPlatform++
		}
	}
	if ownerPlatform <= len(codes) {
		t.Fatalf("OwnerPlatform has %d codes, whitelist has %d; if they are equal the "+
			"Owner-vs-whitelist hazard documented in availability.go no longer exists",
			ownerPlatform, len(codes))
	}

	if IsPlatformAvailabilityCode("") || IsPlatformAvailabilityCode("made_up_code") {
		t.Fatalf("an unknown code must not count as a platform failure")
	}
}

// TestPlatformErrorRateRuleMatchesWhitelist keeps the PromQL regex and the Go
// whitelist identical. PromQL cannot import Go, so rules.yaml repeats the eight
// codes; this test is the only thing stopping the alert from measuring a
// different number than PlatformErrorRateCodes defines.
func TestPlatformErrorRateRuleMatchesWhitelist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "obs", "alert", "rules.yaml"))
	if err != nil {
		t.Fatalf("read rules.yaml: %v", err)
	}
	yaml := string(raw)

	start := strings.Index(yaml, "- alert: PlatformErrorRateHigh")
	if start < 0 {
		t.Fatalf("PlatformErrorRateHigh rule not found in rules.yaml")
	}
	block := yaml[start:]
	if next := strings.Index(block[1:], "\n      - alert: "); next >= 0 {
		block = block[:next+1]
	}

	re := regexp.MustCompile(`err_code=~"([^"]+)"`)
	match := re.FindStringSubmatch(block)
	if match == nil {
		t.Fatalf("PlatformErrorRateHigh has no err_code=~\"...\" matcher:\n%s", block)
	}

	got := strings.Split(match[1], "|")
	want := PlatformAvailabilityCodes()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("PlatformErrorRateHigh measures a different code set than the whitelist:\n"+
			"  rules.yaml: %v\n  apierr:     %v", got, want)
	}
}

// TestErrorCodesDocListsWhitelist keeps the generated doc in sync. docs/
// error-codes.md is produced by MarkdownTable(); someone regenerating it after
// a whitelist change and forgetting to re-run the generator would leave the
// published table lying.
func TestErrorCodesDocListsWhitelist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "error-codes.md"))
	if err != nil {
		t.Fatalf("read docs/error-codes.md: %v", err)
	}
	doc := string(raw)

	idx := strings.Index(doc, "## 平台可用性白名单")
	if idx < 0 {
		t.Fatalf("docs/error-codes.md has no 平台可用性白名单 section; regenerate it from " +
			"apierr.MarkdownTable()")
	}
	section := doc[idx:]

	// Collect the codes of the section's own table (rows look like
	// "| `code` | 触发阶段 X |"), so a stale table cannot hide behind prose.
	var listed []string
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "| `") || strings.HasPrefix(trimmed, "| `code` |") {
			continue
		}
		rest := trimmed[2:] // starts at the opening backtick
		closeAt := strings.Index(rest[1:], "`")
		if closeAt < 0 {
			continue
		}
		listed = append(listed, rest[1:1+closeAt])
	}
	want := PlatformAvailabilityCodes()
	got := append([]string(nil), listed...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("docs/error-codes.md's whitelist table lists %v; the code says %v. "+
			"Regenerate the file from apierr.MarkdownTable()", got, want)
	}
	if !strings.Contains(section, "`"+CodeUpstreamKeyUnavailable+"`") {
		t.Fatalf("the whitelist section no longer explains which upstream codes are excluded; " +
			"a reader would 'fix' the doc by adding them back")
	}
}

// TestMarkdownAvailabilitySectionIsGenerated exercises the generator itself.
//
// TestErrorCodesDocListsWhitelist only proves the checked-in file matches; if
// MarkdownAvailabilitySection() started emitting a broken table, the doc would
// still pass until someone regenerated it. This is also the only caller that
// keeps the function off 0% coverage, which the apierr gate is sensitive to
// (it sits at 90%).
func TestMarkdownAvailabilitySectionIsGenerated(t *testing.T) {
	section := MarkdownAvailabilitySection()
	if !strings.Contains(section, "PRD §9.3") {
		t.Fatalf("generated section does not name PRD §9.3:\n%s", section)
	}
	for _, code := range PlatformAvailabilityCodes() {
		if !strings.Contains(section, "| `"+code+"` |") {
			t.Fatalf("generated section has no table row for whitelisted code %s:\n%s", code, section)
		}
	}
	// The four OwnerPlatform-but-not-whitelisted codes must appear in prose at
	// most; a table row for one of them would publish the wrong numerator.
	for _, code := range []string{
		CodePriceNotConfigured,
		CodeUpstreamAuthFailed,
		CodeUpstreamModelNotFound,
		CodeUpstreamKeyUnavailable,
	} {
		if strings.Contains(section, "| `"+code+"` |") {
			t.Fatalf("%s must not be a row in the availability table (it is not in the "+
				"§9.3 numerator):\n%s", code, section)
		}
	}
}
