// Package config owns the configuration key registry, the immutable per-
// namespace snapshots, the version protocol that keeps them consistent, and
// the C2 strategy registry.
//
// Direction of dependency (T05 hard constraint 1): business packages import
// config to reference key constants and to register strategy implementations.
// config never imports a business package.
package config

// Key is a configuration key. Business code must reference the constants in
// this file; a bare key string literal elsewhere in internal/ is a review
// reject (T05 hard constraint 2). The single metadata table below is the only
// place where the key text appears.
type Key string

// The 45 configuration keys (TDS §3.7; the "34" count in prose is superseded,
// see docs/tasks/errata.md E-07).
const (
	KeyBillingDeductPolicy          Key = "billing.deduct_policy"
	KeyBillingRefundPolicy          Key = "billing.refund_policy"
	KeyBillingOverdraftLimitDefault Key = "billing.overdraft_limit_default"
	KeyBillingEstimateLanguageCoef  Key = "billing.estimate_language_coef"
	KeyBillingPreholdRatio          Key = "billing.prehold_ratio"
	KeyBillingPreholdMaxRatio       Key = "billing.prehold_max_ratio"
	KeyBillingSettleDeadlineMs      Key = "billing.settle_deadline_ms"
	KeyBillingBonusExpireDays       Key = "billing.bonus_expire_days"
	KeyBillingLongCtxGuardBP        Key = "billing.long_ctx_prehold_guard_bp"
	KeyBillingPrestimateSafetyBP    Key = "billing.prestimate_safety_bp"

	KeyPricingCostSource          Key = "pricing.cost_source"
	KeyPricingTierChain           Key = "pricing.tier_chain"
	KeyPricingAgentMarkup         Key = "pricing.agent_markup"
	KeyPricingCacheBillingEnabled Key = "pricing.cache_billing_enabled"
	KeyPricingMinMarginBP         Key = "pricing.min_margin_bp"
	KeyPricingPlanMinMarginBP     Key = "pricing.plan_min_margin_bp"
	KeyPricingTaxRateMultiplier   Key = "pricing.tax_rate_multiplier"
	KeyPricingMaxBonusRatioBP     Key = "pricing.max_bonus_ratio_bp"
	KeyPricingGroupSwitchMode     Key = "pricing.group_switch_mode"

	KeyPlanDefaultTier Key = "plan.default_tier"
	KeyPlanTrialDays   Key = "plan.trial_days"

	KeyQuotaDailyMeter       Key = "quota.daily_meter"
	KeyQuotaKeyDailyMode     Key = "quota.key_daily_mode"
	KeyQuotaResetScope       Key = "quota.reset_scope"
	KeyQuotaResetTZ          Key = "quota.reset_tz"
	KeyQuotaRateLimitOrder   Key = "quota.rate_limit_order"
	KeyQuotaFreeMaxActiveKey Key = "quota.free_max_active_keys"

	KeyLogBodyMode      Key = "log.body_mode"
	KeyLogVolumeBasis   Key = "log.volume_basis"
	KeyLogRetentionDays Key = "log.retention_days"
	KeyLogSameInstance  Key = "log.same_instance"

	KeyIdemDuplicateWindowSeconds Key = "idem.duplicate_window_seconds"
	KeyIdemLeaseTTLSeconds        Key = "idem.lease_ttl_seconds"
	KeyIdemLeaseRenewSeconds      Key = "idem.lease_renew_seconds"

	KeySiteRecommendLimit Key = "site.recommend_limit"

	KeyGwMaxInflight          Key = "gw.max_inflight"
	KeyGwDrainDeadlineMs      Key = "gw.drain_deadline_ms"
	KeyGwUpstreamTimeoutMs    Key = "gw.upstream_timeout_ms"
	KeyGwStreamFirstByteMs    Key = "gw.stream_first_byte_ms"
	KeyGwStreamTotalMs        Key = "gw.stream_total_ms"
	KeyGwSettleReserveMs      Key = "gw.settle_reserve_ms"
	KeyGwRetryThresholdMs     Key = "gw.retry_threshold_ms"
	KeyGwClientTimeoutBudget  Key = "gw.client_timeout_budget_ms"
	KeyGwRetryMax             Key = "gw.retry_max"
	KeyQuotaOverdraftWarnDays Key = "quota.overdraft_warn_days"
)

// EnvLogBodyKEK is the request-body envelope KEK (base64 of exactly 32
// bytes). It is deliberately separate from CHANNEL_KEY_KEK: the two ciphertext
// families use different AADs, so an upstream key envelope cannot be opened
// as a request body or the reverse. An empty value disables body storage
// without blocking startup; it is an environment variable, not a Key, so it
// does not change KeyCount.
const EnvLogBodyKEK = "LOG_BODY_KEK"

// EnvInternalCallSecret signs controld's request to the gateway's internal
// playground entry (T27). It is an environment variable, not a configuration
// Key: a signing secret in t_config_item would be readable by every process
// that loads configuration. Both daemons must read the same value; a missing
// value keeps the gateway route unregistered and makes controld answer 503.
const EnvInternalCallSecret = "INTERNAL_CALL_SECRET"

// EnvPhoneHMACKey is the key HMAC-SHA256'd with a canonicalised phone number to
// produce t_user.phone_hmac. Like the other secrets here it is an environment
// variable and never a configuration Key: a secret in t_config_item is readable
// by every process that loads configuration, and the 45 frozen keys carry none.
//
// It must NOT reuse FP_HMAC_KEY (device fingerprints). A phone number is an
// 11-digit, ~10^9 space: an attacker holding both the database and the key can
// enumerate every stored number back to plaintext in minutes, which makes this
// key equivalent to the PII it protects. The fingerprint input space is
// client-chosen and high-entropy, so a leak there cannot be reversed the same
// way — different classification, different key, different lifecycle.
//
// Missing or shorter than 32 bytes: the process still starts, phone *writes*
// answer 503 config_unavailable, and login/recovery (which read the plaintext
// phone column) are unaffected.
const EnvPhoneHMACKey = "PHONE_HMAC_KEY"

// EnvGatewayInternalURL is gatewayd's in-network base URL as seen from
// controld (compose: http://gatewayd:8080). Empty disables the relay with 503.
const EnvGatewayInternalURL = "GATEWAY_INTERNAL_URL"

// KeyCount is asserted against the metadata table and the t_config_item seed.
const KeyCount = 45

// RuntimeKeyCount is the size of the runtime-writable whitelist. TDS §3.7
// fixed it at 12; TA-1.1's four new keys are deliberately outside it.
const RuntimeKeyCount = 12

// Namespace is one of the five snapshot partitions plus the deliberately
// unassigned bucket.
type Namespace string

// Namespaces. There is no sixth snapshot namespace: adding one would add a
// new invalidation channel (TDS §3.7, errata E-08).
const (
	NsRouting    Namespace = "ROUTING"
	NsRateLimit  Namespace = "RATELIMIT"
	NsPricing    Namespace = "PRICING"
	NsUpstream   Namespace = "UPSTREAM"
	NsFeature    Namespace = "FEATURE"
	NsUnassigned Namespace = "UNASSIGNED"
)

// SnapshotNamespaces is the ordered list of namespaces that own a snapshot.
// NsUnassigned is intentionally absent: quota.overdraft_warn_days must never
// be loaded into a snapshot that nobody reads (T05 hard constraint 3).
var SnapshotNamespaces = [...]Namespace{NsRouting, NsRateLimit, NsPricing, NsUpstream, NsFeature}

// ValueType is the validation type of a configuration value.
type ValueType string

// Value types.
const (
	TypeInt     ValueType = "int"
	TypeBool    ValueType = "bool"
	TypeEnum    ValueType = "enum"
	TypeDecimal ValueType = "decimal"
	TypeJSON    ValueType = "json"
	TypeString  ValueType = "string"
)

// Enum values accepted by the V1.0 definition domains. String literals live
// here so business packages never repeat them.
const (
	EnumFifoExpiry    = "fifo_expiry"
	EnumProrata       = "prorata"
	EnumRechargeFirst = "recharge_first"

	EnumByOrder       = "by_order"
	EnumByAccount     = "by_account"
	EnumClearanceOnly = "clearance_only"

	EnumManual     = "manual"
	EnumBillImport = "bill_import"

	EnumPrehold = "prehold"
	EnumSettle  = "settle"

	EnumNone    = "none"
	EnumInherit = "inherit"
	EnumStrict  = "strict"

	EnumAccount = "account"
	EnumKey     = "key"

	EnumAsiaShanghai = "Asia/Shanghai"
	EnumOrderDefault = "concurrency>rpm>tpm>daily"

	EnumMetaOnly = "meta_only"
	EnumOptIn    = "opt_in"

	EnumVolumeMeta      = "meta"
	EnumVolumeBody      = "body"
	EnumVolumeAllTables = "all_tables"

	EnumTierModelChannel = "model>channel"
	EnumAgentMarkupOff   = "off"
	EnumGroupOpsOnly     = "ops_only"
)

// IntRange is an inclusive integer bound.
type IntRange struct {
	Min int64
	Max int64
}

// DecimalRange is an inclusive decimal bound.
type DecimalRange struct {
	Min Decimal
	Max Decimal
}

// FieldSpec describes one required member of a JSON object value.
type FieldSpec struct {
	Name string
	Type ValueType // TypeInt or TypeDecimal only.
	Int  IntRange
	Dec  DecimalRange
}

// KeyMeta is the single source of truth for one configuration key: type,
// default, namespace, runtime writability and C2 strategy binding.
//
// Do not add per-key conditionals elsewhere. A key that needs special
// treatment gets a field here (T05 forbidden item 2).
type KeyMeta struct {
	Key Key

	Type    ValueType
	Default string // JSON literal; must match the 0018 seed semantically.
	MaxLen  int    // TypeString only.

	Namespace Namespace

	RuntimeWritable bool
	RuntimeEnum     []string // non-empty restricts runtime writes to these values.

	Enum   []string // TypeEnum / TypeString definition domain.
	Int    *IntRange
	Dec    *DecimalRange
	Fields []FieldSpec

	Strategy StrategyKind // non-zero when this key selects a C2 implementation.
}

func intRange(min, max int64) *IntRange { return &IntRange{Min: min, Max: max} }

func decRange(min, max Decimal) *DecimalRange { return &DecimalRange{Min: min, Max: max} }

// keyTable is the only list of configuration keys in the repository.
//
// Order follows docs/config-keys.md: the 34 base keys, then the 11 keys
// appended by TA-1.2. The namespace column follows TDS §3.7 and errata E-08.
var keyTable = []KeyMeta{
	// PRICING: billing.* (10)
	{
		Key: KeyBillingDeductPolicy, Type: TypeEnum, Default: `"fifo_expiry"`,
		Namespace: NsPricing, RuntimeWritable: true,
		Enum: []string{EnumFifoExpiry, EnumProrata, EnumRechargeFirst}, Strategy: KindDeductPolicy,
	},
	{
		Key: KeyBillingRefundPolicy, Type: TypeEnum, Default: `"by_order"`,
		Namespace: NsPricing,
		Enum:      []string{EnumByOrder, EnumByAccount, EnumClearanceOnly}, Strategy: KindRefundPolicy,
	},
	{
		Key: KeyBillingOverdraftLimitDefault, Type: TypeInt, Default: `50`,
		Namespace: NsPricing, Int: intRange(0, 1_000_000_000),
	},
	{
		Key: KeyBillingEstimateLanguageCoef, Type: TypeJSON,
		Default:   `{"zh":1.0,"en":1.0,"code":1.0,"mixed":1.0}`,
		Namespace: NsPricing,
		Fields: []FieldSpec{
			{Name: "zh", Type: TypeDecimal, Dec: DecimalRange{Min: 0, Max: 10 * DecimalScale}},
			{Name: "en", Type: TypeDecimal, Dec: DecimalRange{Min: 0, Max: 10 * DecimalScale}},
			{Name: "code", Type: TypeDecimal, Dec: DecimalRange{Min: 0, Max: 10 * DecimalScale}},
			{Name: "mixed", Type: TypeDecimal, Dec: DecimalRange{Min: 0, Max: 10 * DecimalScale}},
		},
	},
	{
		Key: KeyBillingPreholdRatio, Type: TypeDecimal, Default: `1.2`,
		Namespace: NsPricing, Dec: decRange(1*DecimalScale, 10*DecimalScale),
	},
	{
		Key: KeyBillingPreholdMaxRatio, Type: TypeDecimal, Default: `0.5`,
		Namespace: NsPricing, RuntimeWritable: true, Dec: decRange(0, 1*DecimalScale),
	},
	{
		Key: KeyBillingSettleDeadlineMs, Type: TypeJSON,
		Default:   `{"non_stream_ms":5000,"stream_ms":10000}`,
		Namespace: NsPricing,
		Fields: []FieldSpec{
			{Name: "non_stream_ms", Type: TypeInt, Int: IntRange{Min: 0, Max: 600_000}},
			{Name: "stream_ms", Type: TypeInt, Int: IntRange{Min: 0, Max: 600_000}},
		},
	},
	{
		Key: KeyBillingBonusExpireDays, Type: TypeInt, Default: `90`,
		Namespace: NsPricing, Int: intRange(0, 3650),
	},
	{
		Key: KeyBillingLongCtxGuardBP, Type: TypeInt, Default: `9000`,
		Namespace: NsPricing, Int: intRange(0, 10_000),
	},
	{
		// The safety margin is deliberately above 100% (11000 bp = +10%).
		Key: KeyBillingPrestimateSafetyBP, Type: TypeInt, Default: `11000`,
		Namespace: NsPricing, Int: intRange(10_000, 20_000),
	},

	// PRICING: pricing.* (9)
	{
		Key: KeyPricingCostSource, Type: TypeEnum, Default: `"manual"`,
		Namespace: NsPricing,
		Enum:      []string{EnumManual, EnumBillImport}, Strategy: KindCostSource,
	},
	{
		// V1.0 accepts only model>channel. The TDS reserves
		// model>protocol>channel>agent for V2.0; adding it here without a
		// registered implementation would fail the startup gate.
		Key: KeyPricingTierChain, Type: TypeEnum, Default: `"model>channel"`,
		Namespace: NsPricing,
		Enum:      []string{EnumTierModelChannel}, Strategy: KindTierChain,
	},
	{
		Key: KeyPricingAgentMarkup, Type: TypeEnum, Default: `"off"`,
		Namespace: NsPricing, Enum: []string{EnumAgentMarkupOff},
	},
	{
		Key: KeyPricingCacheBillingEnabled, Type: TypeBool, Default: `false`,
		Namespace: NsPricing,
	},
	{
		Key: KeyPricingMinMarginBP, Type: TypeInt, Default: `2500`,
		Namespace: NsPricing, Int: intRange(0, 10_000),
	},
	{
		Key: KeyPricingPlanMinMarginBP, Type: TypeInt, Default: `1000`,
		Namespace: NsPricing, Int: intRange(0, 10_000),
	},
	{
		Key: KeyPricingTaxRateMultiplier, Type: TypeDecimal, Default: `1.00`,
		Namespace: NsPricing, Dec: decRange(0, 10*DecimalScale),
	},
	{
		Key: KeyPricingMaxBonusRatioBP, Type: TypeInt, Default: `3000`,
		Namespace: NsPricing, Int: intRange(0, 10_000),
	},
	{
		Key: KeyPricingGroupSwitchMode, Type: TypeEnum, Default: `"ops_only"`,
		Namespace: NsPricing,
		Enum:      []string{EnumGroupOpsOnly}, Strategy: KindGroupSwitchMode,
	},

	// PRICING: plan.* (2)
	{
		Key: KeyPlanDefaultTier, Type: TypeString, Default: `"free"`,
		Namespace: NsPricing, MaxLen: 64,
	},
	{
		Key: KeyPlanTrialDays, Type: TypeInt, Default: `7`,
		Namespace: NsPricing, Int: intRange(0, 365),
	},

	// RATELIMIT: quota.* (6)
	{
		Key: KeyQuotaDailyMeter, Type: TypeEnum, Default: `"prehold"`,
		Namespace: NsRateLimit, RuntimeWritable: true,
		Enum: []string{EnumPrehold, EnumSettle}, Strategy: KindDailyMeter,
	},
	{
		Key: KeyQuotaKeyDailyMode, Type: TypeEnum, Default: `"none"`,
		Namespace: NsRateLimit, RuntimeWritable: true,
		RuntimeEnum: []string{EnumNone},
		Enum:        []string{EnumNone, EnumInherit, EnumStrict}, Strategy: KindKeyDailyQuota,
	},
	{
		Key: KeyQuotaResetScope, Type: TypeEnum, Default: `"account"`,
		Namespace: NsRateLimit, RuntimeWritable: true,
		RuntimeEnum: []string{EnumAccount},
		Enum:        []string{EnumAccount, EnumKey}, Strategy: KindResetScope,
	},
	{
		Key: KeyQuotaResetTZ, Type: TypeString, Default: `"Asia/Shanghai"`,
		Namespace: NsRateLimit, RuntimeWritable: true,
		RuntimeEnum: []string{EnumAsiaShanghai}, Enum: []string{EnumAsiaShanghai}, MaxLen: 64,
	},
	{
		Key: KeyQuotaRateLimitOrder, Type: TypeEnum, Default: `"concurrency>rpm>tpm>daily"`,
		Namespace: NsRateLimit, RuntimeWritable: true,
		RuntimeEnum: []string{EnumOrderDefault}, Enum: []string{EnumOrderDefault},
	},
	{
		Key: KeyQuotaFreeMaxActiveKey, Type: TypeInt, Default: `3`,
		Namespace: NsRateLimit, Int: intRange(0, 1000),
	},

	// FEATURE: log.* (4)
	{
		Key: KeyLogBodyMode, Type: TypeEnum, Default: `"meta_only"`,
		Namespace: NsFeature, Enum: []string{EnumMetaOnly, EnumOptIn},
	},
	{
		Key: KeyLogVolumeBasis, Type: TypeEnum, Default: `"meta"`,
		Namespace: NsFeature, Enum: []string{EnumVolumeMeta, EnumVolumeBody, EnumVolumeAllTables},
	},
	{
		Key: KeyLogRetentionDays, Type: TypeInt, Default: `180`,
		Namespace: NsFeature, RuntimeWritable: true, Int: intRange(1, 3650),
	},
	{
		Key: KeyLogSameInstance, Type: TypeBool, Default: `true`,
		Namespace: NsFeature,
	},

	// FEATURE: idem.* (3) — errata E-08 assigns feature switches here.
	{
		// 0 is a legal value: it switches automatic de-duplication off for
		// requests without an Idempotency-Key, which P08 expresses as
		// window <= 0. The default stays 10s.
		Key: KeyIdemDuplicateWindowSeconds, Type: TypeInt, Default: `10`,
		Namespace: NsFeature, Int: intRange(0, 3600),
	},
	{
		Key: KeyIdemLeaseTTLSeconds, Type: TypeInt, Default: `15`,
		Namespace: NsFeature, Int: intRange(1, 3600),
	},
	{
		Key: KeyIdemLeaseRenewSeconds, Type: TypeInt, Default: `5`,
		Namespace: NsFeature, Int: intRange(1, 3600),
	},

	// FEATURE: site.* (1) — errata E-08.
	{
		Key: KeySiteRecommendLimit, Type: TypeInt, Default: `8`,
		Namespace: NsFeature, Int: intRange(1, 100),
	},

	// UPSTREAM: gw.* (9)
	{
		Key: KeyGwMaxInflight, Type: TypeInt, Default: `8000`,
		Namespace: NsUpstream, RuntimeWritable: true, Int: intRange(1, 1_000_000),
	},
	{
		Key: KeyGwDrainDeadlineMs, Type: TypeInt, Default: `120000`,
		Namespace: NsUpstream, RuntimeWritable: true, Int: intRange(0, 3_600_000),
	},
	{
		Key: KeyGwUpstreamTimeoutMs, Type: TypeInt, Default: `50000`,
		Namespace: NsUpstream, RuntimeWritable: true, Int: intRange(1, 3_600_000),
	},
	{
		Key: KeyGwStreamFirstByteMs, Type: TypeInt, Default: `30000`,
		Namespace: NsUpstream, Int: intRange(1, 3_600_000),
	},
	{
		Key: KeyGwStreamTotalMs, Type: TypeInt, Default: `600000`,
		Namespace: NsUpstream, Int: intRange(1, 86_400_000),
	},
	{
		Key: KeyGwSettleReserveMs, Type: TypeInt, Default: `15000`,
		Namespace: NsUpstream, Int: intRange(0, 3_600_000),
	},
	{
		Key: KeyGwRetryThresholdMs, Type: TypeJSON,
		Default:   `{"unary":65000,"stream":45000}`,
		Namespace: NsUpstream,
		Fields: []FieldSpec{
			{Name: "unary", Type: TypeInt, Int: IntRange{Min: 0, Max: 3_600_000}},
			{Name: "stream", Type: TypeInt, Int: IntRange{Min: 0, Max: 3_600_000}},
		},
	},
	{
		Key: KeyGwClientTimeoutBudget, Type: TypeInt, Default: `120000`,
		Namespace: NsUpstream, RuntimeWritable: true, Int: intRange(1, 86_400_000),
	},
	{
		Key: KeyGwRetryMax, Type: TypeInt, Default: `1`,
		Namespace: NsUpstream, Int: intRange(0, 5),
	},

	// UNASSIGNED (1). This key must never enter a snapshot.
	{
		Key: KeyQuotaOverdraftWarnDays, Type: TypeInt, Default: `7`,
		Namespace: NsUnassigned, Int: intRange(0, 365),
	},
}

var (
	metaByKey   map[Key]*KeyMeta
	allKeys     []Key
	runtimeKeys []Key
)

func init() {
	metaByKey = make(map[Key]*KeyMeta, len(keyTable))
	for i := range keyTable {
		m := &keyTable[i]
		if m.Key == "" {
			panic("config: metadata entry with empty key")
		}
		if _, dup := metaByKey[m.Key]; dup {
			panic("config: duplicate key metadata for " + string(m.Key))
		}
		if m.Namespace == "" {
			panic("config: missing namespace for " + string(m.Key))
		}
		if m.Type == TypeEnum && len(m.Enum) == 0 {
			panic("config: enum key without domain " + string(m.Key))
		}
		metaByKey[m.Key] = m
		allKeys = append(allKeys, m.Key)
		if m.RuntimeWritable {
			runtimeKeys = append(runtimeKeys, m.Key)
		}
		// A malformed default would be served to every node. Fail at package
		// init rather than at the first request.
		if _, err := Parse(m.Key, []byte(m.Default)); err != nil {
			panic("config: invalid default for " + string(m.Key) + ": " + err.Error())
		}
	}
	if len(keyTable) != KeyCount {
		panic("config: metadata table does not contain 45 keys")
	}
	if len(runtimeKeys) != RuntimeKeyCount {
		panic("config: runtime whitelist does not contain 12 keys")
	}
}

// Meta returns the metadata for key. Slices and range pointers are copied so
// a caller cannot mutate the package table.
func Meta(key Key) (KeyMeta, bool) {
	m, ok := metaByKey[key]
	if !ok {
		return KeyMeta{}, false
	}
	out := *m
	out.Enum = append([]string(nil), m.Enum...)
	out.RuntimeEnum = append([]string(nil), m.RuntimeEnum...)
	out.Fields = append([]FieldSpec(nil), m.Fields...)
	if m.Int != nil {
		r := *m.Int
		out.Int = &r
	}
	if m.Dec != nil {
		r := *m.Dec
		out.Dec = &r
	}
	return out, true
}

// AllKeys returns every key in table order. The caller receives a copy.
func AllKeys() []Key {
	out := make([]Key, len(allKeys))
	copy(out, allKeys)
	return out
}

// KeysInNamespace returns the keys that belong to ns in table order.
func KeysInNamespace(ns Namespace) []Key {
	out := make([]Key, 0, 8)
	for _, k := range allKeys {
		if metaByKey[k].Namespace == ns {
			out = append(out, k)
		}
	}
	return out
}

// RuntimeWritable reports whether /admin/config may write this key.
func RuntimeWritable(key Key) bool {
	m, ok := metaByKey[key]
	return ok && m.RuntimeWritable
}

// RuntimeWritableKeys returns the 12 whitelisted keys. The caller receives a
// copy.
func RuntimeWritableKeys() []Key {
	out := make([]Key, len(runtimeKeys))
	copy(out, runtimeKeys)
	return out
}

// namespaceIndex maps a snapshot namespace to its atomic pointer slot.
func namespaceIndex(ns Namespace) int {
	for i, candidate := range SnapshotNamespaces {
		if candidate == ns {
			return i
		}
	}
	return -1
}
