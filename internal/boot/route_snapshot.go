// Package boot produces the routing snapshot and the gateway caches.
//
// T15 owns the operational endpoints for these rebuilds. The first segment
// needs one reusable builder plus a periodic tick so a fresh local database
// can actually route a request: before this package, cfg:snap:ROUTING had no
// producer at all.
package boot

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/jsonx"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// RouteStats describes one build. It is returned so seedctl and the periodic
// refresher can log what they published without reading Redis back.
type RouteStats struct {
	Models    int
	Aliases   int
	Routes    int
	Channels  int
	Groups    int
	Keys      int
	KeyModels int
	Bytes     int
	Took      time.Duration
}

// The wire structs mirror gw.ModelEntry / RouteEntry / ChannelEntry etc.
// They are duplicated rather than imported because gw's bool fields use an
// unexported flexBool type that another package cannot construct. The JSON
// tags are the contract; the gateway's decoder accepts plain booleans.
type modelEntry struct {
	ID                 int64  `json:"id"`
	ModelName          string `json:"model_name"`
	Region             string `json:"region"`
	ModelType          string `json:"model_type"`
	ContextLength      int64  `json:"context_length"`
	MaxOutputTokens    int64  `json:"max_output_tokens"`
	ListingState       string `json:"listing_state"`
	DefaultPriceRuleID int64  `json:"default_price_rule_id"`
	CapabilityMask     int64  `json:"capability_mask"`
}

type routeEntry struct {
	ID                int64  `json:"id"`
	ModelID           int64  `json:"model_id"`
	ChannelID         int64  `json:"channel_id"`
	ProtocolID        int64  `json:"protocol_id"`
	UpstreamModelName string `json:"upstream_model_name"`
	Region            string `json:"region"`
	Weight            int64  `json:"weight"`
	Priority          int64  `json:"priority"`
	StreamSupported   bool   `json:"stream_supported"`
	MaxTokensCeiling  int64  `json:"max_tokens_ceiling"`
	Enabled           bool   `json:"enabled"`
}

type channelEntry struct {
	ID                   int64             `json:"id"`
	Name                 string            `json:"name"`
	ProtocolID           int64             `json:"protocol_id"`
	Region               string            `json:"region"`
	Weight               int64             `json:"weight"`
	Priority             int64             `json:"priority"`
	GroupScopeMode       string            `json:"group_scope_mode"`
	DedicatedScopeMode   string            `json:"dedicated_scope_mode"`
	FallbackAllowed      bool              `json:"fallback_allowed"`
	StreamSupported      bool              `json:"stream_supported"`
	Enabled              bool              `json:"enabled"`
	BaseURL              string            `json:"base_url"`
	UpstreamKeyEnc       string            `json:"upstream_key_enc"`
	TimeoutConnectMS     int64             `json:"timeout_connect_ms"`
	TimeoutFirstByteMS   int64             `json:"timeout_first_byte_ms"`
	TimeoutTotalMS       int64             `json:"timeout_total_ms"`
	RetryMax             *int64            `json:"retry_max"`
	StreamUsageMode      string            `json:"stream_usage_mode"`
	UpstreamModelNameMap map[string]string `json:"upstream_model_name_map"`
}

type groupEntry struct {
	Code             string  `json:"code"`
	RatioBP          int64   `json:"ratio_bp"`
	ModelScopeMode   string  `json:"model_scope_mode"`
	ChannelScopeMode string  `json:"channel_scope_mode"`
	Enabled          bool    `json:"enabled"`
	Models           []int64 `json:"models"`
	Channels         []int64 `json:"channels"`
}

type keyModelGrant struct {
	Mode     string  `json:"mode"`
	ModelIDs []int64 `json:"model_ids"`
}

type ipRule struct {
	Version int64    `json:"version"`
	CIDRs   []string `json:"cidrs"`
}

type limitPolicy struct {
	RPM             int64 `json:"rpm"`
	TPM             int64 `json:"tpm"`
	Concurrency     int64 `json:"concurrency"`
	DailyQuotaMicro int64 `json:"daily_quota_micro"`
}

// RouteSnapshot builds and publishes cfg:snap:ROUTING (D-14 payload).
type RouteSnapshot struct {
	DB        *sql.DB
	Publisher *config.Publisher
	Now       clock.Clock
	Logger    *obslog.Logger
}

// Build assembles the payload without publishing it.
func (b *RouteSnapshot) Build(ctx context.Context) ([]byte, RouteStats, error) {
	start := b.now()
	models, aliases, err := b.loadModels(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	routes, err := b.loadRoutes(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	channels, err := b.loadChannels(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	groups, err := b.loadGroups(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	keyModels, keyCount, grantCount, err := b.loadKeyModels(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	dedicated, err := b.loadDedicated(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	ipRules, err := b.loadIPRules(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}
	limits, err := b.loadRateLimits(ctx)
	if err != nil {
		return nil, RouteStats{}, err
	}

	values := map[string]any{
		"models":         models,
		"aliases":        aliases,
		"routes":         routes,
		"channels":       channels,
		"groups":         groups,
		"keyModels":      keyModels,
		"dedicatedUsers": dedicated,
		"ipRules":        ipRules,
		"rateLimits":     limits,
	}
	wire := struct {
		Namespace string         `json:"namespace"`
		Version   int64          `json:"version"`
		Values    map[string]any `json:"values"`
	}{
		Namespace: string(config.NsRouting),
		Values:    values,
	}
	payload, err := jsonx.Marshal(wire)
	if err != nil {
		return nil, RouteStats{}, fmt.Errorf("boot: encode routing snapshot: %w", err)
	}
	stats := RouteStats{
		Models:    len(models),
		Aliases:   len(aliases),
		Routes:    len(routes),
		Channels:  len(channels),
		Groups:    len(groups),
		Keys:      keyCount,
		KeyModels: grantCount,
		Bytes:     len(payload),
		Took:      b.now().Sub(start),
	}
	return payload, stats, nil
}

func (b *RouteSnapshot) now() time.Time {
	if b.Now != nil {
		return b.Now.Now()
	}
	return clock.Real{}.Now()
}

// Publish builds and writes the snapshot through the T05 publisher, which
// performs SET cfg:snap:ROUTING -> INCR cfg:ver -> PUBLISH in that order.
func (b *RouteSnapshot) Publish(ctx context.Context) (RouteStats, *config.PublishResult, error) {
	if b.DB == nil || b.Publisher == nil {
		return RouteStats{}, nil, fmt.Errorf("boot: route snapshot requires database and publisher")
	}
	payload, stats, err := b.Build(ctx)
	if err != nil {
		return stats, nil, err
	}
	result, err := b.Publisher.PublishRaw(ctx, config.NsRouting, payload, nil, 0)
	if err != nil {
		return stats, result, fmt.Errorf("boot: publish routing snapshot: %w", err)
	}
	stats.Bytes = result.SnapshotBytes[config.NsRouting]
	return stats, result, nil
}

// modelEntryColumns is the t_model projection consumed by scanModelEntry, in
// the exact order the scan reads it. Keep the two in step: context_length and
// max_output_tokens are both 64-bit integers and sit next to each other, so a
// mis-ordered scan swaps the two values silently instead of failing.
// TestModelSelectColumnsMatchScanOrder guards the alignment.
//
// max_output_tokens must be projected at all: before T46 it was declared on
// modelEntry but never selected, so every snapshot carried 0 and the gateway's
// second-level output fallback (model's own ceiling) could never fire.
const modelEntryColumns = `id, model_name, region, model_type, context_length,
		       max_output_tokens, default_price_rule_id, capability_mask, listing_state`

// scanModelEntry fills one modelEntry from the modelEntryColumns projection.
// The destination order below is the contract the const above documents.
func scanModelEntry(m *modelEntry, scan func(dest ...any) error) error {
	var priceRef sql.NullInt64
	if err := scan(&m.ID, &m.ModelName, &m.Region, &m.ModelType, &m.ContextLength,
		&m.MaxOutputTokens, &priceRef, &m.CapabilityMask, &m.ListingState); err != nil {
		return err
	}
	if priceRef.Valid {
		m.DefaultPriceRuleID = priceRef.Int64
	}
	return nil
}

func (b *RouteSnapshot) loadModels(ctx context.Context) ([]modelEntry, map[string]string, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT `+modelEntryColumns+`
		  FROM t_model
		 WHERE deleted_at IS NULL
		 ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("boot: load models: %w", err)
	}
	defer rows.Close()

	models := make([]modelEntry, 0, 64)
	for rows.Next() {
		var m modelEntry
		if err := scanModelEntry(&m, rows.Scan); err != nil {
			return nil, nil, fmt.Errorf("boot: scan model: %w", err)
		}
		models = append(models, m)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("boot: iterate models: %w", err)
	}

	aliasRows, err := b.DB.QueryContext(ctx, `
		SELECT a.alias_name, m.model_name
		  FROM t_model_alias a
		  JOIN t_model m ON m.id = a.model_id
		 WHERE a.enabled = 1 AND m.deleted_at IS NULL
		 ORDER BY a.alias_name`)
	if err != nil {
		return nil, nil, fmt.Errorf("boot: load aliases: %w", err)
	}
	defer aliasRows.Close()
	aliases := make(map[string]string, 32)
	for aliasRows.Next() {
		var alias, name string
		if err := aliasRows.Scan(&alias, &name); err != nil {
			return nil, nil, fmt.Errorf("boot: scan alias: %w", err)
		}
		aliases[alias] = name
	}
	if err := aliasRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("boot: iterate aliases: %w", err)
	}
	return models, aliases, nil
}

func (b *RouteSnapshot) loadRoutes(ctx context.Context) ([]routeEntry, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT r.id, r.model_id, r.channel_id, r.protocol_id, r.upstream_model_name,
		       r.region, r.weight, r.priority, r.stream_supported,
		       r.max_tokens_ceiling, r.enabled
		  FROM t_route r
		  JOIN t_model m   ON m.id = r.model_id   AND m.deleted_at IS NULL
		  JOIN t_channel c ON c.id = r.channel_id AND c.deleted_at IS NULL
		 ORDER BY r.model_id, r.priority, r.id`)
	if err != nil {
		return nil, fmt.Errorf("boot: load routes: %w", err)
	}
	defer rows.Close()
	out := make([]routeEntry, 0, 64)
	for rows.Next() {
		var r routeEntry
		if err := rows.Scan(&r.ID, &r.ModelID, &r.ChannelID, &r.ProtocolID,
			&r.UpstreamModelName, &r.Region, &r.Weight, &r.Priority,
			&r.StreamSupported, &r.MaxTokensCeiling, &r.Enabled); err != nil {
			return nil, fmt.Errorf("boot: scan route: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate routes: %w", err)
	}
	return out, nil
}

func (b *RouteSnapshot) loadChannels(ctx context.Context) ([]channelEntry, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT id, name, protocol_id, region, weight, priority,
		       group_scope_mode, dedicated_scope_mode, fallback_allowed,
		       stream_supported, enabled, base_url, upstream_key_enc,
		       timeout_connect_ms, timeout_first_byte_ms, timeout_total_ms,
		       retry_max, stream_usage_mode, upstream_model_name_map
		  FROM t_channel
		 WHERE deleted_at IS NULL
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("boot: load channels: %w", err)
	}
	defer rows.Close()
	out := make([]channelEntry, 0, 16)
	for rows.Next() {
		var c channelEntry
		var keyEnc []byte
		var retry uint8
		var nameMap []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.ProtocolID, &c.Region, &c.Weight,
			&c.Priority, &c.GroupScopeMode, &c.DedicatedScopeMode, &c.FallbackAllowed,
			&c.StreamSupported, &c.Enabled, &c.BaseURL, &keyEnc,
			&c.TimeoutConnectMS, &c.TimeoutFirstByteMS, &c.TimeoutTotalMS,
			&retry, &c.StreamUsageMode, &nameMap); err != nil {
			return nil, fmt.Errorf("boot: scan channel: %w", err)
		}
		// D-14: the snapshot carries the envelope string ("v1:"+base64 of the
		// nonce/ciphertext/tag), not an extra base64 layer. The gateway checks the
		// prefix and calls cryptobox.Open on exactly this value, so base64-encoding
		// the varbinary again would make every channel unusable.
		c.UpstreamKeyEnc = string(keyEnc)
		retryValue := int64(retry)
		c.RetryMax = &retryValue
		// An absent JSON column is still an empty map on the wire: the dispatcher
		// refuses a nil map (D-14 "missing field => refuse") but accepts {}.
		c.UpstreamModelNameMap = make(map[string]string)
		if len(nameMap) > 0 && string(nameMap) != "null" {
			decoded := make(map[string]string)
			if err := jsonx.Unmarshal(nameMap, &decoded); err != nil {
				return nil, fmt.Errorf("boot: channel %d upstream_model_name_map: %w", c.ID, err)
			}
			c.UpstreamModelNameMap = decoded
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate channels: %w", err)
	}
	return out, nil
}

func (b *RouteSnapshot) loadGroups(ctx context.Context) (map[string]groupEntry, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT id, code, ratio_bp, model_scope_mode, channel_scope_mode, enabled
		  FROM t_price_group
		 WHERE enabled = 1
		 ORDER BY code`)
	if err != nil {
		return nil, fmt.Errorf("boot: load groups: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]string, 8)
	groups := make(map[string]groupEntry, 8)
	for rows.Next() {
		var id int64
		var g groupEntry
		if err := rows.Scan(&id, &g.Code, &g.RatioBP, &g.ModelScopeMode,
			&g.ChannelScopeMode, &g.Enabled); err != nil {
			return nil, fmt.Errorf("boot: scan group: %w", err)
		}
		groups[g.Code] = g
		byID[id] = g.Code
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate groups: %w", err)
	}

	scopeRows, err := b.DB.QueryContext(ctx, `
		SELECT gs.group_id, gs.scope_type, gs.scope_id
		  FROM t_price_group_scope gs
		  JOIN t_price_group g ON g.id = gs.group_id AND g.enabled = 1
		 ORDER BY gs.group_id, gs.scope_type, gs.scope_id`)
	if err != nil {
		return nil, fmt.Errorf("boot: load group scopes: %w", err)
	}
	defer scopeRows.Close()
	for scopeRows.Next() {
		var groupID, scopeID int64
		var scopeType string
		if err := scopeRows.Scan(&groupID, &scopeType, &scopeID); err != nil {
			return nil, fmt.Errorf("boot: scan group scope: %w", err)
		}
		code, ok := byID[groupID]
		if !ok {
			continue
		}
		g := groups[code]
		switch scopeType {
		case "MODEL":
			g.Models = append(g.Models, scopeID)
		case "CHANNEL":
			g.Channels = append(g.Channels, scopeID)
		}
		groups[code] = g
	}
	if err := scopeRows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate group scopes: %w", err)
	}
	return groups, nil
}

func (b *RouteSnapshot) loadKeyModels(ctx context.Context) (map[string]keyModelGrant, int, int, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT id, model_scope_mode
		  FROM t_api_key
		 WHERE deleted_at IS NULL
		 ORDER BY id`)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("boot: load keys: %w", err)
	}
	defer rows.Close()

	grants := make(map[string]keyModelGrant, 64)
	keyIDs := make([]int64, 0, 64)
	for rows.Next() {
		var id int64
		var mode string
		if err := rows.Scan(&id, &mode); err != nil {
			return nil, 0, 0, fmt.Errorf("boot: scan key grant: %w", err)
		}
		keyIDs = append(keyIDs, id)
		grants[strKey(id)] = keyModelGrant{Mode: mode}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("boot: iterate key grants: %w", err)
	}

	grantRows, err := b.DB.QueryContext(ctx, `
		SELECT km.api_key_id, km.model_id
		  FROM t_api_key_model km
		  JOIN t_api_key k ON k.id = km.api_key_id AND k.deleted_at IS NULL
		 ORDER BY km.api_key_id, km.model_id`)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("boot: load key model grants: %w", err)
	}
	defer grantRows.Close()
	grantsCount := 0
	for grantRows.Next() {
		var keyID, modelID int64
		if err := grantRows.Scan(&keyID, &modelID); err != nil {
			return nil, 0, 0, fmt.Errorf("boot: scan key model grant: %w", err)
		}
		g := grants[strKey(keyID)]
		if g.Mode == "" {
			g.Mode = "SELECTED"
		}
		g.ModelIDs = append(g.ModelIDs, modelID)
		grants[strKey(keyID)] = g
		grantsCount++
	}
	if err := grantRows.Err(); err != nil {
		return nil, 0, 0, fmt.Errorf("boot: iterate key model grants: %w", err)
	}
	return grants, len(keyIDs), grantsCount, nil
}

func (b *RouteSnapshot) loadDedicated(ctx context.Context) (map[string][]int64, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT channel_id, user_id
		  FROM t_channel_dedicated_user
		 ORDER BY channel_id, user_id`)
	if err != nil {
		return nil, fmt.Errorf("boot: load dedicated users: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]int64, 8)
	for rows.Next() {
		var channelID, userID int64
		if err := rows.Scan(&channelID, &userID); err != nil {
			return nil, fmt.Errorf("boot: scan dedicated user: %w", err)
		}
		out[strKey(channelID)] = append(out[strKey(channelID)], userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate dedicated users: %w", err)
	}
	return out, nil
}

func (b *RouteSnapshot) loadIPRules(ctx context.Context) (map[string]ipRule, error) {
	rows, err := b.DB.QueryContext(ctx, `
		SELECT id, api_key_id, cidr
		  FROM t_api_key_ip_rule
		 ORDER BY api_key_id, id`)
	if err != nil {
		return nil, fmt.Errorf("boot: load ip rules: %w", err)
	}
	defer rows.Close()
	out := make(map[string]ipRule, 8)
	for rows.Next() {
		var id, keyID int64
		var cidr string
		if err := rows.Scan(&id, &keyID, &cidr); err != nil {
			return nil, fmt.Errorf("boot: scan ip rule: %w", err)
		}
		rule := out[strKey(keyID)]
		rule.Version = id
		rule.CIDRs = append(rule.CIDRs, cidr)
		out[strKey(keyID)] = rule
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate ip rules: %w", err)
	}
	return out, nil
}

func (b *RouteSnapshot) loadRateLimits(ctx context.Context) (map[string]limitPolicy, error) {
	now := clock.Real{}.Now().UTC()
	if b.Now != nil {
		now = b.Now.Now().UTC()
	}
	rows, err := b.DB.QueryContext(ctx, `
		SELECT scope_type, scope_id, rpm, tpm, concurrency_quota, daily_quota_micro
		  FROM t_rate_limit_policy
		 WHERE effective_from <= ?
		   AND (effective_to IS NULL OR effective_to > ?)
		 ORDER BY scope_type, scope_id, effective_from ASC`,
		now, now)
	if err != nil {
		return nil, fmt.Errorf("boot: load rate limits: %w", err)
	}
	defer rows.Close()
	out := make(map[string]limitPolicy, 32)
	for rows.Next() {
		var scopeType string
		var scopeID int64
		var p limitPolicy
		var daily sql.NullInt64
		if err := rows.Scan(&scopeType, &scopeID, &p.RPM, &p.TPM,
			&p.Concurrency, &daily); err != nil {
			return nil, fmt.Errorf("boot: scan rate limit: %w", err)
		}
		if daily.Valid {
			p.DailyQuotaMicro = daily.Int64
		}
		// Later rows overwrite earlier ones because the query is ordered by
		// effective_from: the newest active policy wins.
		out[scopeType+":"+strKey(scopeID)] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("boot: iterate rate limits: %w", err)
	}
	return out, nil
}

func strKey(id int64) string { return strconv.FormatInt(id, 10) }

// Refresher periodically republishes the routing snapshot and refreshes the
// K19/K13 caches. Intervals are chosen below the Redis TTLs: K19 is 120s, the
// routing snapshot is 300s.
type Refresher struct {
	Routes     *RouteSnapshot
	Caches     *CacheWriter
	RouteEvery time.Duration
	CacheEvery time.Duration
	Logger     *obslog.Logger
}

// Run blocks until ctx is canceled.
func (r *Refresher) Run(ctx context.Context) {
	routeEvery := r.RouteEvery
	if routeEvery <= 0 {
		routeEvery = 4 * time.Minute
	}
	cacheEvery := r.CacheEvery
	if cacheEvery <= 0 {
		cacheEvery = time.Minute
	}
	r.tick(ctx)
	route := time.NewTicker(routeEvery)
	cache := time.NewTicker(cacheEvery)
	defer route.Stop()
	defer cache.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-route.C:
			r.tick(ctx)
		case <-cache.C:
			if r.Caches != nil {
				if err := r.Caches.Refresh(ctx); err != nil && r.Logger != nil {
					r.Logger.Warn(ctx, "boot_cache_refresh_failed: "+err.Error(), obslog.Fields{Stage: "boot"})
				}
			}
		}
	}
}

func (r *Refresher) tick(ctx context.Context) {
	if r.Routes != nil {
		stats, _, err := r.Routes.Publish(ctx)
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn(ctx, "boot_route_publish_failed: "+err.Error(), obslog.Fields{Stage: "boot"})
			}
		} else if r.Logger != nil {
			r.Logger.Info(ctx, fmt.Sprintf("boot_route_published models=%d routes=%d channels=%d keys=%d bytes=%d",
				stats.Models, stats.Routes, stats.Channels, stats.Keys, stats.Bytes),
				obslog.Fields{Stage: "boot"})
		}
	}
	if r.Caches != nil {
		if err := r.Caches.Refresh(ctx); err != nil && r.Logger != nil {
			r.Logger.Warn(ctx, "boot_cache_refresh_failed: "+err.Error(), obslog.Fields{Stage: "boot"})
		}
	}
}
