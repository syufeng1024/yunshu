// The platform-availability whitelist (PRD §9.3).
//
// PRD §9.3 measures gateway availability as
//
//	1 − (平台侧故障请求数 ÷ 到达网关的全部请求)
//
// and it fixes the numerator to **eight** codes, listed below. The spec names
// them "按 `Spec.Owner = platform` 白名单 8 条码计", which reads like "filter by
// Owner" — it is not, and this file exists because the two are different sets
// (G26):
//
//   - 12 specs carry Owner == OwnerPlatform (see codes.go), and four of them
//     are NOT in the whitelist: `price_not_configured` (a real platform fault,
//     but P09 pricing misconfiguration is counted as a deployment defect, not
//     gateway unavailability) plus the three upstream-credential codes
//     `upstream_auth_failed` / `upstream_model_not_found` /
//     `upstream_key_unavailable`, which PRD §9.3 requires to be observed as
//     "上游依赖可用性" instead of being folded into the platform number.
//   - Owner is consumed by exactly one producer, `gw.usage.ownerForCode`, to
//     fill `t_usage_record.fail_owner` on inbound (pre-dispatch) failures. That
//     column is attribution metadata read by the request-log APIs; no ledger,
//     settlement or billing code reads it, so flipping Owner would not move
//     money — but it also would not make `Owner == PLATFORM` equal to the
//     whitelist, because `price_not_configured` would still be inside.
//     Worse, `dispatch.attribution` (the other writer of fail_owner) assigns
//     `upstream_key_unavailable` to PLATFORM/ROUTE, so changing Owner alone
//     would leave the same code with two different owners depending on which
//     pipeline stage failed.
//
// So the whitelist is data, not a filter on Owner: it is the single definition
// the availability numerator must use. `internal/obs/alert/rules.yaml` repeats
// it as a PromQL regex (PromQL cannot import Go); TestPlatformErrorRateRuleMatchesWhitelist
// asserts the two sets are identical, so this list is the only place to edit.
package apierr

// platformAvailabilityCodes is PRD §9.3's numerator, in the order the spec
// lists them. Changing this list is a change to a published availability
// number: it needs the same sign-off as the SLO itself, not a code review.
var platformAvailabilityCodes = [...]string{
	CodeRateLimiterUnavailable,     // 限流 Redis 不可用（fail-close 503）
	CodeConfigUnavailable,          // 配置中心不可达且无可用快照（E-60）
	CodeAuthUnavailable,            // K13 鉴权不可用
	CodeSettlementUnavailable,      // P10 预扣不可用
	CodeBackpressureRejected,       // 进程背压 >90%
	CodeBackpressureStreamRejected, // 进程背压 60%..90%，仅流式
	CodeInternalError,              // 全局兜底
	CodeConvertFailed,              // P14/P20 协议转换失败
}

// PlatformAvailabilityCodes returns the §9.3 numerator codes. The slice is a
// copy: a caller that sorts or truncates it must not change the definition
// every other caller sees.
func PlatformAvailabilityCodes() []string {
	out := make([]string, len(platformAvailabilityCodes))
	copy(out, platformAvailabilityCodes[:])
	return out
}

// IsPlatformAvailabilityCode reports whether code counts in the §9.3 numerator.
// An unknown or empty code is not a platform failure: a caller that invents a
// code must not be able to inflate the platform's own unavailability.
//
// This is the predicate to use instead of `spec.Owner == OwnerPlatform`
// anywhere availability is computed.
func IsPlatformAvailabilityCode(code string) bool {
	for _, c := range platformAvailabilityCodes {
		if c == code {
			return true
		}
	}
	return false
}
