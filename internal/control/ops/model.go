package ops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

// Listing states (TDS t_model.listing_state).
const (
	listingListed      = "LISTED"
	listingCatalogOnly = "CATALOG_ONLY"
	listingDelisted    = "DELISTED"
)

// recommendWeightMax bounds t_model.recommend_weight. The column is an
// unsigned int, so without a business cap an operator can send 2^31-1 and make
// one model dominate every recommendation slot. 1000 keeps the value on the
// same scale as the console's percentage/basis-point fields and leaves room for
// relative weighting without turning the sort key into an arbitrary number.
const recommendWeightMax int64 = 1000

func validateRecommendWeight(v int64) *apierr.APIError {
	if v < 0 || v > recommendWeightMax {
		return mw.InvalidField("recommend_weight")
	}
	return nil
}

type modelCapabilities struct {
	Stream   *bool `json:"stream"`
	ToolCall *bool `json:"tool_call"`
	Vision   *bool `json:"vision"`
	JSONMode *bool `json:"json_mode"`
}

// modelPricingView is read-only in T15. Price writes belong to the pricing
// segment, including the 7-day notice and the gross-margin floor.
type modelPricingView struct {
	RuleID                 int64     `json:"price_rule_id"`
	Version                int64     `json:"version"`
	EffectiveFrom          time.Time `json:"effective_from"`
	InPriceMicroPerK       int64     `json:"in_price_micro_per_k"`
	OutPriceMicroPerK      int64     `json:"out_price_micro_per_k"`
	PerCallPriceMicro      int64     `json:"per_call_price_micro"`
	CacheHitRatioBP        *int64    `json:"cache_hit_ratio_bp,omitempty"`
	CacheWriteRatioBP      *int64    `json:"cache_write_ratio_bp,omitempty"`
	LongCtxThresholdTokens *int64    `json:"long_ctx_threshold_tokens,omitempty"`
	LongCtxInPriceMicro    *int64    `json:"long_ctx_in_price_micro_per_k,omitempty"`
	LongCtxOutPriceMicro   *int64    `json:"long_ctx_out_price_micro_per_k,omitempty"`
	CostInMicroPerK        *int64    `json:"cost_in_micro_per_k,omitempty"`
	CostOutMicroPerK       *int64    `json:"cost_out_micro_per_k,omitempty"`
	CostPerCallMicro       *int64    `json:"cost_per_call_micro,omitempty"`
}

type modelView struct {
	ID             int64  `json:"id"`
	ModelName      string `json:"model_name"`
	DisplayName    string `json:"display_name"`
	Region         string `json:"region"`
	ModelType      string `json:"model_type"`
	ProtocolID     int64  `json:"protocol_id"`
	Protocol       string `json:"protocol"`
	PrimaryChannel *int64 `json:"primary_channel_id,omitempty"`
	ContextLength  int64  `json:"context_length"`
	// MaxOutputTokens is the model's own output ceiling. 0 means "not
	// configured" — the gateway then falls back to min(context_length,
	// PlatformMaxOutputTokens). It is the operator's input for that fallback,
	// not the platform cap the client's max_tokens is clamped to.
	MaxOutputTokens int64             `json:"max_output_tokens"`
	CapabilityMask  int64             `json:"capability_mask"`
	Capabilities    modelCapabilities `json:"capabilities"`
	ListingState    string            `json:"listing_state"`
	RecommendWeight int64             `json:"recommend_weight"`
	PublishedAt     *time.Time        `json:"published_at,omitempty"`
	DelistedAt      *time.Time        `json:"delisted_at,omitempty"`
	Aliases         []string          `json:"aliases"`
	Pricing         *modelPricingView `json:"pricing"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type modelListResponse struct {
	Items    []modelView `json:"items"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	Total    int64       `json:"total"`
}

type modelDetailResponse struct {
	Model modelView `json:"model"`
}

type modelCreateRequest struct {
	ModelName        *string          `json:"model_name"`
	DisplayName      *string          `json:"display_name"`
	Region           *string          `json:"region"`
	ModelType        *string          `json:"model_type"`
	ProtocolID       *int64           `json:"protocol_id"`
	PrimaryChannelID *int64           `json:"primary_channel_id"`
	ContextLength    *int64           `json:"context_length"`
	MaxOutputTokens  *int64           `json:"max_output_tokens"`
	ListingState     *string          `json:"listing_state"`
	RecommendWeight  *int64           `json:"recommend_weight"`
	Aliases          *[]string        `json:"aliases"`
	Capabilities     *map[string]bool `json:"capabilities"`
	// T45: when both are set, a default t_route row is created in the same
	// transaction. upstream_model_name alone (without primary_channel_id) is a
	// 400 — guessing a channel would bind the model to the wrong upstream.
	UpstreamModelName *string `json:"upstream_model_name"`
	StreamSupported   *bool   `json:"stream_supported"`
}

type modelPatchRequest struct {
	// ModelName is the single source of truth every other surface reads
	// (pricing table, model catalogue, routing). Renaming here is the only
	// way to change it (F22-1).
	ModelName        *string          `json:"model_name"`
	DisplayName      *string          `json:"display_name"`
	Region           *string          `json:"region"`
	ModelType        *string          `json:"model_type"`
	ProtocolID       *int64           `json:"protocol_id"`
	PrimaryChannelID *int64           `json:"primary_channel_id"`
	ContextLength    *int64           `json:"context_length"`
	MaxOutputTokens  *int64           `json:"max_output_tokens"`
	ListingState     *string          `json:"listing_state"`
	RecommendWeight  *int64           `json:"recommend_weight"`
	Aliases          *[]string        `json:"aliases"`
	Capabilities     *map[string]bool `json:"capabilities"`
	Reason           string           `json:"reason"`
}

type listingStateRequest struct {
	ListingState *string `json:"listing_state"`
	Reason       string  `json:"reason"`
}

type recommendWeightRequest struct {
	RecommendWeight *int64 `json:"recommend_weight"`
	Reason          string `json:"reason"`
}

const modelSelectColumns = `
	m.id, m.model_name, m.display_name, m.region, m.model_type, m.protocol_id, p.code,
	m.primary_channel_id, m.context_length, m.max_output_tokens, m.capability_mask,
	m.listing_state, m.recommend_weight, m.published_at, m.delisted_at, m.created_at,
	m.updated_at`

// ListModels implements GET /api/admin/models.
func (s *Service) ListModels(w http.ResponseWriter, r *http.Request) {
	page, apiErr := mw.ParsePage(r, "created_at", "updated_at", "model_name", "recommend_weight", "published_at")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("q"))
	if len([]rune(search)) > 128 {
		mw.WriteError(w, r, mw.InvalidQueryField("q"))
		return
	}
	modelType := strings.ToUpper(strings.TrimSpace(q.Get("model_type")))
	if modelType != "" && modelType != "TEXT" && modelType != "IMAGE" && modelType != "EMBEDDING" {
		mw.WriteError(w, r, mw.InvalidQueryField("model_type"))
		return
	}
	state := strings.ToUpper(strings.TrimSpace(q.Get("listing_state")))
	if state != "" && !validListingState(state) {
		mw.WriteError(w, r, mw.InvalidQueryField("listing_state"))
		return
	}
	region := strings.ToUpper(strings.TrimSpace(q.Get("region")))
	if region != "" && region != regionDomestic && region != regionOverseas {
		mw.WriteError(w, r, mw.InvalidQueryField("region"))
		return
	}
	protocol := strings.ToUpper(strings.TrimSpace(q.Get("protocol")))
	if len(protocol) > 32 {
		mw.WriteError(w, r, mw.InvalidQueryField("protocol"))
		return
	}

	where := []string{"m.deleted_at IS NULL"}
	args := make([]any, 0, 6)
	if search != "" {
		pattern := likePattern(search)
		where = append(where, `(m.model_name LIKE ? ESCAPE '!' OR m.display_name LIKE ? ESCAPE '!'
			OR EXISTS (SELECT 1 FROM t_model_alias a WHERE a.model_id = m.id AND a.alias_name LIKE ? ESCAPE '!'))`)
		args = append(args, pattern, pattern, pattern)
	}
	if modelType != "" {
		where = append(where, "m.model_type = ?")
		args = append(args, modelType)
	}
	if state != "" {
		where = append(where, "m.listing_state = ?")
		args = append(args, state)
	}
	if region != "" {
		where = append(where, "m.region = ?")
		args = append(args, region)
	}
	if protocol != "" {
		where = append(where, "p.code = ?")
		args = append(args, protocol)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := s.DB.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(r.Context(), "ops_model_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(r.Context(), `
		SELECT `+modelSelectColumns+`
		  FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE `+whereSQL+`
		 ORDER BY `+modelOrder(page)+` LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.logError(r.Context(), "ops_model_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]modelView, 0, page.Size)
	ids := make([]int64, 0, page.Size)
	for rows.Next() {
		item, err := scanModel(rows)
		if err != nil {
			s.logError(r.Context(), "ops_model_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		items = append(items, item)
		ids = append(ids, item.ID)
	}
	if err := rows.Err(); err != nil {
		s.logError(r.Context(), "ops_model_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := s.attachModelDetails(r.Context(), s.DB, items, ids); err != nil {
		s.logError(r.Context(), "ops_model_details_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, modelListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// GetModel implements GET /api/admin/models/{id}.
func (s *Service) GetModel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	view, apiErr := s.loadModel(r.Context(), s.DB, id, false)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, modelDetailResponse{Model: view})
}

// CreateModel implements POST /api/admin/models.
func (s *Service) CreateModel(w http.ResponseWriter, r *http.Request) {
	var req modelCreateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	in, apiErr := s.validateCreateModel(r, &req)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	ctx := r.Context()
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_model_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM t_model WHERE model_name = ? LIMIT 1`,
		in.modelName).Scan(&existing)
	if err == nil {
		mw.WriteError(w, r, apierr.Newf(apierr.CodeInvalidRequestBody,
			fmt.Sprintf("模型名 %s 已存在（id=%d）", in.modelName, existing)))
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.logError(ctx, "ops_model_name_check_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if apiErr := aliasesAvailable(ctx, tx, in.aliases, 0); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if apiErr := primaryChannelValid(ctx, tx, in.primaryChannelID); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	var publishedAt, delistedAt any
	if in.listingState == listingListed {
		publishedAt = now
	}
	if in.listingState == listingDelisted {
		delistedAt = now
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_model
			(model_name, display_name, region, model_type, protocol_id, primary_channel_id,
			 context_length, max_output_tokens, capability_mask, listing_state, recommend_weight,
			 published_at, delisted_at, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		in.modelName, in.displayName, in.region, in.modelType, in.protocolID,
		nullableInt64(in.primaryChannelID), in.contextLength, in.maxOutputTokens, in.capabilityMask,
		in.listingState, in.recommendWeight, publishedAt, delistedAt, now, now)
	if err != nil {
		s.logError(ctx, "ops_model_insert_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	id, err := res.LastInsertId()
	if err != nil {
		s.logError(ctx, "ops_model_id_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	// F16-7: a model may only be LISTED with an effective price. The id exists
	// only after the INSERT, so the check runs here; the deferred rollback
	// removes the row when it refuses.
	if in.listingState == listingListed {
		if apiErr := s.requireEffectivePrice(ctx, tx, id); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	// T45: bind the new model to its primary channel so it can actually be
	// called after listing. region is copied from the channel, never from the
	// request: the gateway selects routes by region and a mismatched copy would
	// make this route invisible. insertRoute defaults weight/priority to 100,
	// max_tokens_ceiling to 0 and enabled to 1.
	routeCreated := false
	if in.upstreamModelName != "" {
		region, protocolID, apiErr := loadChannelRegionProtocol(ctx, tx, *in.primaryChannelID, "primary_channel_id")
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		created, err := insertRoute(ctx, tx, id, *in.primaryChannelID, protocolID,
			in.upstreamModelName, region, in.streamSupported, now)
		if err != nil {
			s.logError(ctx, "ops_model_route_insert_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		routeCreated = created
	}
	if apiErr := replaceAliases(ctx, tx, id, in.aliases, now); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if apiErr := replaceCapabilities(ctx, tx, id, in.capabilities, now); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	view, apiErr := s.loadModelTx(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	afterAudit := modelAuditView(view)
	if routeCreated {
		afterAudit["route_created"] = true
		afterAudit["upstream_model_name"] = in.upstreamModelName
		afterAudit["stream_supported"] = in.streamSupported
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", "CREATE", "t_model", id, nil,
		afterAudit, ""); err != nil {
		s.logError(ctx, "ops_model_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_model_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	// A new route is invisible to the gateway until the snapshot is republished,
	// exactly like a listing-state change.
	if routeCreated || in.listingState == listingListed {
		if apiErr := s.publish(ctx); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	_ = mw.WriteJSON(w, http.StatusCreated, modelDetailResponse{Model: view})
}

// UpdateModel implements PATCH /api/admin/models/{id}. Listing-state changes
// are accepted here as well; the dedicated endpoint exists so the console can
// show a confirmation dialog without sending the whole object.
func (s *Service) UpdateModel(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req modelPatchRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if !hasModelPatch(&req) {
		mw.WriteError(w, r, mw.InvalidField("body"))
		return
	}
	s.updateModel(w, r, id, &req)
}

// SetListingState implements POST /api/admin/models/{id}/listing-state.
func (s *Service) SetListingState(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req listingStateRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if req.ListingState == nil {
		mw.WriteError(w, r, mw.InvalidField("listing_state"))
		return
	}
	s.updateModel(w, r, id, &modelPatchRequest{ListingState: req.ListingState, Reason: req.Reason})
}

// SetRecommendWeight implements POST /api/admin/models/{id}/recommend-weight.
func (s *Service) SetRecommendWeight(w http.ResponseWriter, r *http.Request) {
	id, apiErr := pathID(r, "id")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	var req recommendWeightRequest
	if apiErr := decodeJSON(w, r, &req); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if req.RecommendWeight == nil {
		mw.WriteError(w, r, mw.InvalidField("recommend_weight"))
		return
	}
	if apiErr := validateRecommendWeight(*req.RecommendWeight); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_model_weight_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadModelTxForUpdate(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if *req.RecommendWeight > 0 && before.ListingState != listingListed {
		mw.WriteError(w, r, recommendWeightRefusal(before.ListingState))
		return
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE t_model SET recommend_weight = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL`,
		*req.RecommendWeight, s.now(), id); err != nil {
		s.logError(ctx, "ops_model_weight_update_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	after, apiErr := s.loadModelTx(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", "UPDATE", "t_model", id,
		modelAuditView(before), modelAuditView(after), req.Reason); err != nil {
		s.logError(ctx, "ops_model_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_model_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true
	_ = mw.WriteJSON(w, http.StatusOK, modelDetailResponse{Model: after})
}

// updateModel is the common path for PATCH and the listing-state endpoint.
func (s *Service) updateModel(w http.ResponseWriter, r *http.Request, id int64, req *modelPatchRequest) {
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.logError(ctx, "ops_model_patch_begin_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	before, apiErr := s.loadModelTxForUpdate(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	now := s.now()
	sets := make([]string, 0, 10)
	args := make([]any, 0, 12)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}

	if req.ModelName != nil {
		name := strings.TrimSpace(*req.ModelName)
		if name == "" || len([]rune(name)) > 128 {
			mw.WriteError(w, r, mw.InvalidField("model_name"))
			return
		}
		if name != before.ModelName {
			// Renaming rewrites the one row every other read joins to. A name
			// already used by another model, or already used as another
			// model's alias, is refused with both ids in the message (F22-2).
			if apiErr := modelNameAvailable(ctx, tx, name, id); apiErr != nil {
				mw.WriteError(w, r, apiErr)
				return
			}
			add("model_name", name)
		}
	}
	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if name == "" || len([]rune(name)) > 128 {
			mw.WriteError(w, r, mw.InvalidField("display_name"))
			return
		}
		add("display_name", name)
	}
	if req.Region != nil {
		region, apiErr := validateRegion(req.Region, "region", "模型")
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("region", region)
	}
	if req.ModelType != nil {
		mt, apiErr := validateModelType(*req.ModelType)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("model_type", mt)
	}
	if req.ProtocolID != nil {
		if *req.ProtocolID <= 0 || !protocolExists(ctx, tx, *req.ProtocolID) {
			mw.WriteError(w, r, mw.InvalidField("protocol_id"))
			return
		}
		add("protocol_id", *req.ProtocolID)
	}
	if req.PrimaryChannelID != nil {
		if apiErr := primaryChannelValid(ctx, tx, req.PrimaryChannelID); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("primary_channel_id", nullableInt64(req.PrimaryChannelID))
	}
	if req.ContextLength != nil {
		if *req.ContextLength < 0 || *req.ContextLength > 100_000_000 {
			mw.WriteError(w, r, mw.InvalidField("context_length"))
			return
		}
		add("context_length", *req.ContextLength)
	}
	if req.MaxOutputTokens != nil {
		// Validate against the context length this request will leave behind:
		// a PATCH may raise the ceiling and the context length together.
		nextContextLength := before.ContextLength
		if req.ContextLength != nil {
			nextContextLength = *req.ContextLength
		}
		if apiErr := validateMaxOutputTokens(*req.MaxOutputTokens, nextContextLength); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("max_output_tokens", *req.MaxOutputTokens)
	}
	if req.Capabilities != nil {
		mask, apiErr := capabilityMask(*req.Capabilities)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		add("capability_mask", mask)
	}
	if req.Aliases != nil {
		aliases, apiErr := normalizeAliases(*req.Aliases)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		if apiErr := aliasesAvailable(ctx, tx, aliases, id); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	nextState := before.ListingState
	stateChanged := false
	if req.ListingState != nil {
		state, apiErr := validateListingState(*req.ListingState)
		if apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
		nextState = state
		stateChanged = state != before.ListingState
		add("listing_state", state)
	}

	// F16-7: switching into LISTED requires a price that is effective now.
	// DELISTED / CATALOG_ONLY stay reachable without a price, so an unpriced
	// model can always be taken off the shelf.
	if stateChanged && nextState == listingListed {
		if apiErr := s.requireEffectivePrice(ctx, tx, id); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	// Recommendation weight: non-zero is meaningful only for LISTED. A move
	// away from LISTED zeroes it automatically, even when the caller did not
	// mention the field; that is the documented behaviour, not a silent cap.
	if req.RecommendWeight != nil {
		if apiErr := validateRecommendWeight(*req.RecommendWeight); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	weight := before.RecommendWeight
	if req.RecommendWeight != nil {
		weight = *req.RecommendWeight
	}
	if nextState != listingListed {
		if req.RecommendWeight != nil && *req.RecommendWeight > 0 {
			mw.WriteError(w, r, recommendWeightRefusal(nextState))
			return
		}
		weight = 0
	}
	if req.RecommendWeight != nil || weight != before.RecommendWeight {
		add("recommend_weight", weight)
	}

	if stateChanged {
		switch {
		case nextState == listingListed:
			if before.PublishedAt == nil {
				add("published_at", now)
			}
			add("delisted_at", nil)
		case nextState == listingDelisted:
			add("delisted_at", now)
		}
	}

	// PRD F17-1 / F17-3 name exactly these two transitions: LISTED →
	// CATALOG_ONLY (no notice period) and CATALOG_ONLY → LISTED (a "new
	// listing" draft). Other transitions are audited but generate nothing.
	var announcementID int64
	needsDraft := stateChanged &&
		((before.ListingState == listingListed && nextState == listingCatalogOnly) ||
			(before.ListingState == listingCatalogOnly && nextState == listingListed))
	if needsDraft {
		title, content := listingAnnouncementCopy(before, nextState)
		announcementID, err = insertAnnouncementDraft(ctx, tx, title, content, mustActorID(r), now)
		if err != nil {
			s.logError(ctx, "ops_model_announcement_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		// The draft is a write of its own; audit it separately so an operator
		// reading the announcement trail sees where the row came from. The
		// draft row itself has created_by but no reason column (0015-era
		// t_announcement), so the operator's reason is recorded here and in
		// the model audit row below.
		draftReason := strings.TrimSpace(req.Reason)
		if draftReason == "" {
			draftReason = "由模型状态变更自动生成"
		}
		if err := s.insertAudit(ctx, tx, r, "ANNOUNCEMENT", "CREATE", "t_announcement",
			announcementID, nil, map[string]any{
				"title": title, "state": "DRAFT", "position_mask": 2,
				"listing_state": nextState, "operator_id": mustActorID(r),
				"reason": draftReason,
			}, draftReason); err != nil {
			s.logError(ctx, "ops_model_announcement_audit_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}

	if len(sets) > 0 {
		add("updated_at", now)
		if _, err := tx.ExecContext(ctx,
			`UPDATE t_model SET `+strings.Join(sets, ", ")+` WHERE id = ? AND deleted_at IS NULL`,
			append(args, id)...); err != nil {
			s.logError(ctx, "ops_model_update_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
	}
	if req.Aliases != nil {
		aliases, _ := normalizeAliases(*req.Aliases)
		if apiErr := replaceAliases(ctx, tx, id, aliases, now); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	if req.Capabilities != nil {
		if apiErr := replaceCapabilities(ctx, tx, id, *req.Capabilities, now); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}

	after, apiErr := s.loadModelTx(ctx, tx, id)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	afterMap := modelAuditView(after)
	if announcementID > 0 {
		afterMap["announcement_id"] = announcementID
	}
	action := "UPDATE"
	if stateChanged {
		action = "LISTING_STATE_CHANGE"
	}
	if err := s.insertAudit(ctx, tx, r, "MODEL", action, "t_model", id,
		modelAuditView(before), afterMap, req.Reason); err != nil {
		s.logError(ctx, "ops_model_audit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	if err := tx.Commit(); err != nil {
		s.logError(ctx, "ops_model_commit_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	committed = true

	if stateChanged {
		if apiErr := s.publish(ctx); apiErr != nil {
			mw.WriteError(w, r, apiErr)
			return
		}
	}
	_ = mw.WriteJSON(w, http.StatusOK, modelDetailResponse{Model: after})
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

type modelInput struct {
	modelName        string
	displayName      string
	region           string
	modelType        string
	protocolID       int64
	primaryChannelID *int64
	contextLength    int64
	maxOutputTokens  int64
	listingState     string
	recommendWeight  int64
	aliases          []string
	capabilities     map[string]bool
	capabilityMask   int64
	// upstreamModelName is empty when no default route was requested.
	upstreamModelName string
	streamSupported   bool
}

func (s *Service) validateCreateModel(r *http.Request, req *modelCreateRequest) (modelInput, *apierr.APIError) {
	var in modelInput
	if req.ModelName == nil {
		return in, mw.InvalidField("model_name")
	}
	name := strings.TrimSpace(*req.ModelName)
	if name == "" || len([]rune(name)) > 128 {
		return in, mw.InvalidField("model_name")
	}
	if req.DisplayName == nil {
		return in, mw.InvalidField("display_name")
	}
	display := strings.TrimSpace(*req.DisplayName)
	if display == "" || len([]rune(display)) > 128 {
		return in, mw.InvalidField("display_name")
	}
	region, apiErr := validateRegion(req.Region, "region", "模型")
	if apiErr != nil {
		return in, apiErr
	}
	if req.ModelType == nil {
		return in, mw.InvalidField("model_type")
	}
	mt, apiErr := validateModelType(*req.ModelType)
	if apiErr != nil {
		return in, apiErr
	}
	if req.ProtocolID == nil || *req.ProtocolID <= 0 {
		return in, mw.InvalidField("protocol_id")
	}
	// New models default to CATALOG_ONLY. A brand-new row cannot have a price
	// rule yet (t_price_rule.scope_id points at an existing t_model.id), so
	// defaulting to LISTED would make every plain create a 409. An explicit
	// LISTED is still accepted once an effective price exists.
	state := listingCatalogOnly
	if req.ListingState != nil {
		var e *apierr.APIError
		state, e = validateListingState(*req.ListingState)
		if e != nil {
			return in, e
		}
	}
	weight := int64(0)
	if req.RecommendWeight != nil {
		if apiErr := validateRecommendWeight(*req.RecommendWeight); apiErr != nil {
			return in, apiErr
		}
		weight = *req.RecommendWeight
	}
	if state != listingListed && weight > 0 {
		return in, recommendWeightRefusal(state)
	}
	contextLength := int64(0)
	if req.ContextLength != nil {
		if *req.ContextLength < 0 || *req.ContextLength > 100_000_000 {
			return in, mw.InvalidField("context_length")
		}
		contextLength = *req.ContextLength
	}
	// max_output_tokens defaults to 0 (= "not configured"), which keeps the
	// gateway's third-level fallback min(context_length, 32768) in charge.
	maxOutputTokens := int64(0)
	if req.MaxOutputTokens != nil {
		if apiErr := validateMaxOutputTokens(*req.MaxOutputTokens, contextLength); apiErr != nil {
			return in, apiErr
		}
		maxOutputTokens = *req.MaxOutputTokens
	}
	var aliases []string
	if req.Aliases != nil {
		var e *apierr.APIError
		aliases, e = normalizeAliases(*req.Aliases)
		if e != nil {
			return in, e
		}
	}
	capabilities := map[string]bool{}
	mask := int64(0)
	if req.Capabilities != nil {
		var e *apierr.APIError
		mask, e = capabilityMask(*req.Capabilities)
		if e != nil {
			return in, e
		}
		capabilities = *req.Capabilities
	}
	// T45: a default route needs a target channel. Refusing here (instead of
	// creating the model without a route) keeps "created but uncallable" — the
	// exact bug this card fixes — impossible to reach through the console.
	upstreamModelName := ""
	if req.UpstreamModelName != nil {
		upstreamModelName = strings.TrimSpace(*req.UpstreamModelName)
		if upstreamModelName == "" || len(upstreamModelName) > 128 {
			return in, mw.InvalidField("upstream_model_name")
		}
		if req.PrimaryChannelID == nil {
			return in, mw.InvalidField("primary_channel_id")
		}
	}
	streamSupported := true
	if req.StreamSupported != nil {
		streamSupported = *req.StreamSupported
	}
	in = modelInput{
		modelName:         name,
		displayName:       display,
		region:            region,
		modelType:         mt,
		protocolID:        *req.ProtocolID,
		primaryChannelID:  req.PrimaryChannelID,
		contextLength:     contextLength,
		maxOutputTokens:   maxOutputTokens,
		listingState:      state,
		recommendWeight:   weight,
		aliases:           aliases,
		capabilities:      capabilities,
		capabilityMask:    mask,
		upstreamModelName: upstreamModelName,
		streamSupported:   streamSupported,
	}
	if !protocolExists(r.Context(), s.DB, in.protocolID) {
		return in, mw.InvalidField("protocol_id")
	}
	if apiErr := primaryChannelValid(r.Context(), s.DB, in.primaryChannelID); apiErr != nil {
		return in, apiErr
	}
	return in, nil
}

// maxOutputTokensCeiling bounds max_output_tokens when the model's
// context_length is unknown (0). It matches the context_length ceiling above so
// the console cannot store a value the column type never expects.
const maxOutputTokensCeiling int64 = 100_000_000

// validateMaxOutputTokens enforces 0 <= value <= contextLength.
//
// Semantics copied from t_model.max_output_tokens itself: 0 means "not
// configured", so it is always accepted. When context_length is known (> 0) the
// output ceiling may not exceed it — context_length is input + output, and a
// ceiling above it would make the gateway reserve more output than the model can
// ever produce. When context_length is 0 (unknown) only the sign is checked:
// refusing there would force operators to record a context length they may not
// have yet.
//
// Deliberately NOT bounded by the platform cap (32768, PlatformMaxOutputTokens
// in internal/gw): that constant clamps the client's max_tokens request, while
// this column records a model capability. Conflating the two would silently
// flatten every long-output model to the platform ceiling.
func validateMaxOutputTokens(value, contextLength int64) *apierr.APIError {
	if value < 0 || value > maxOutputTokensCeiling {
		return mw.InvalidField("max_output_tokens")
	}
	if contextLength > 0 && value > contextLength {
		return mw.InvalidField("max_output_tokens")
	}
	return nil
}

func validateListingState(raw string) (string, *apierr.APIError) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	if !validListingState(v) {
		return "", mw.InvalidField("listing_state")
	}
	return v, nil
}

func validListingState(v string) bool {
	return v == listingListed || v == listingCatalogOnly || v == listingDelisted
}

func validateModelType(raw string) (string, *apierr.APIError) {
	v := strings.ToUpper(strings.TrimSpace(raw))
	switch v {
	case "TEXT", "IMAGE", "EMBEDDING":
		return v, nil
	default:
		return "", mw.InvalidField("model_type")
	}
}

func recommendWeightRefusal(state string) *apierr.APIError {
	return apierr.Newf(apierr.CodeResourceStateConflict,
		fmt.Sprintf("模型当前状态为 %s，推荐权重必须为 0（仅 LISTED 可设非 0 权重）", state))
}

// requireEffectivePrice enforces PRD F16-7: LISTED needs a price that is
// effective at the server's now. It calls activePriceRule (pricing.go) rather
// than repeating the window, so the listing gate and the pricing form cannot
// disagree about what "current price" means: enabled=1, effective_from <= now,
// effective_to IS NULL OR > now, newest version wins. A rule whose
// effective_from is in the future is not a current price and does not pass.
func (s *Service) requireEffectivePrice(ctx context.Context, q priceDB, modelID int64) *apierr.APIError {
	_, err := activePriceRule(ctx, q, modelID, s.now())
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.logError(ctx, "ops_model_price_check_failed: "+err.Error())
		return apierr.New(apierr.CodeInternalError)
	}
	return listingPriceRequired()
}

// listingPriceRequired is a state refusal, not a body error: editing the
// request cannot help, the operator has to configure a price first.
func listingPriceRequired() *apierr.APIError {
	return apierr.Newf(apierr.CodeResourceStateConflict,
		"模型尚未配置生效价格，不能上架").
		WithHint("控制台 → 定价页为该模型配置生效价格后再上架")
}

func normalizeAliases(raw []string) ([]string, *apierr.APIError) {
	if len(raw) > 20 {
		return nil, mw.InvalidField("aliases")
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, a := range raw {
		a = strings.TrimSpace(a)
		if a == "" || len([]rune(a)) > 128 {
			return nil, mw.InvalidField("aliases")
		}
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out, nil
}

func capabilityMask(caps map[string]bool) (int64, *apierr.APIError) {
	var mask int64
	for key, on := range caps {
		bit, ok := map[string]uint{
			"stream": 0, "tool_call": 1, "vision": 2, "json_mode": 3,
		}[key]
		if !ok {
			return 0, mw.InvalidField("capabilities")
		}
		if on {
			mask |= 1 << bit
		}
	}
	return mask, nil
}

// aliasesAvailable refuses an alias already owned by another model, or one
// that equals another model's public name (PRD F22-2: aliases and public names
// may not cross-occupy). The collation is utf8mb4_0900_as_cs, so qwen-Max and
// qwen-max are distinct aliases; no extra normalization happens here.
func aliasesAvailable(ctx context.Context, q modelQueryer, aliases []string, modelID int64) *apierr.APIError {
	for _, a := range aliases {
		ref, found, apiErr := aliasOwner(ctx, q, a, modelID)
		if apiErr != nil {
			return apiErr
		}
		if found {
			return aliasTaken(a, ref)
		}
		ref, found, apiErr = modelNameOwner(ctx, q, a, modelID)
		if apiErr != nil {
			return apiErr
		}
		if found {
			return aliasClashesWithName(a, ref)
		}
	}
	return nil
}

// primaryChannelValid checks existence and the same domestic-only rule as the
// channel form. A nil id means "no primary channel".
func primaryChannelValid(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, id *int64) *apierr.APIError {
	if id == nil {
		return nil
	}
	if *id <= 0 {
		return mw.InvalidField("primary_channel_id")
	}
	var region string
	err := q.QueryRowContext(ctx,
		`SELECT region FROM t_channel WHERE id = ? AND deleted_at IS NULL`, *id).Scan(&region)
	if errors.Is(err, sql.ErrNoRows) {
		return mw.InvalidField("primary_channel_id")
	}
	if err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	if region != regionDomestic {
		return apierr.Newf(apierr.CodeInvalidRequestBody,
			"V1.0 仅支持境内渠道，primary_channel_id 指向的渠道不是境内")
	}
	return nil
}

func hasModelPatch(req *modelPatchRequest) bool {
	return req.ModelName != nil || req.DisplayName != nil || req.Region != nil || req.ModelType != nil ||
		req.ProtocolID != nil || req.PrimaryChannelID != nil || req.ContextLength != nil ||
		req.MaxOutputTokens != nil ||
		req.ListingState != nil || req.RecommendWeight != nil ||
		req.Aliases != nil || req.Capabilities != nil
}

// ---------------------------------------------------------------------------
// storage helpers
// ---------------------------------------------------------------------------

func scanModel(row rowScanner) (modelView, error) {
	var (
		v         modelView
		primary   sql.NullInt64
		published sql.NullTime
		delisted  sql.NullTime
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(
		&v.ID, &v.ModelName, &v.DisplayName, &v.Region, &v.ModelType,
		&v.ProtocolID, &v.Protocol, &primary, &v.ContextLength, &v.MaxOutputTokens,
		&v.CapabilityMask, &v.ListingState, &v.RecommendWeight,
		&published, &delisted, &createdAt, &updatedAt,
	); err != nil {
		return modelView{}, err
	}
	if primary.Valid {
		id := primary.Int64
		v.PrimaryChannel = &id
	}
	v.PublishedAt = nullableTime(published)
	v.DelistedAt = nullableTime(delisted)
	v.CreatedAt = createdAt.UTC()
	v.UpdatedAt = updatedAt.UTC()
	v.Aliases = []string{}
	return v, nil
}

// attachModelDetails fills aliases, capabilities and the effective price rule.
func (s *Service) attachModelDetails(ctx context.Context, q dbQuerier, items []modelView, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	aliases, err := loadAliases(ctx, q, ids)
	if err != nil {
		return err
	}
	caps, err := loadCapabilities(ctx, q, ids)
	if err != nil {
		return err
	}
	prices, err := loadEffectivePrices(ctx, q, ids, s.now())
	if err != nil {
		return err
	}
	for i := range items {
		id := items[i].ID
		if a, ok := aliases[id]; ok {
			items[i].Aliases = a
		}
		items[i].Capabilities = buildCapabilities(items[i].CapabilityMask, caps[id])
		if p, ok := prices[id]; ok {
			items[i].Pricing = &p
		}
	}
	return nil
}

func (s *Service) loadModel(ctx context.Context, q *sql.DB, id int64, includeDeleted bool) (modelView, *apierr.APIError) {
	where := "m.id = ? AND m.deleted_at IS NULL"
	if includeDeleted {
		where = "m.id = ?"
	}
	row := q.QueryRowContext(ctx, `
		SELECT `+modelSelectColumns+`
		  FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE `+where, id)
	view, err := scanModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		// The admin surface addresses rows by id; a missing or soft-deleted
		// model is a 404, not the data plane's 400 model_not_found.
		return modelView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		s.logError(ctx, "ops_model_load_failed: "+err.Error())
		return modelView{}, apierr.New(apierr.CodeInternalError)
	}
	items := []modelView{view}
	if err := s.attachModelDetails(ctx, q, items, []int64{id}); err != nil {
		s.logError(ctx, "ops_model_details_failed: "+err.Error())
		return modelView{}, apierr.New(apierr.CodeInternalError)
	}
	return items[0], nil
}

func (s *Service) loadModelTx(ctx context.Context, tx *sql.Tx, id int64) (modelView, *apierr.APIError) {
	return s.loadModelScoped(ctx, tx, id, false)
}

func (s *Service) loadModelTxForUpdate(ctx context.Context, tx *sql.Tx, id int64) (modelView, *apierr.APIError) {
	return s.loadModelScoped(ctx, tx, id, true)
}

func (s *Service) loadModelScoped(ctx context.Context, tx *sql.Tx, id int64, forUpdate bool) (modelView, *apierr.APIError) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE"
	}
	row := tx.QueryRowContext(ctx, `
		SELECT `+modelSelectColumns+`
		  FROM t_model m JOIN t_protocol p ON p.id = m.protocol_id
		 WHERE m.id = ? AND m.deleted_at IS NULL`+suffix, id)
	view, err := scanModel(row)
	if errors.Is(err, sql.ErrNoRows) {
		return modelView{}, apierr.New(apierr.CodeResourceNotFound)
	}
	if err != nil {
		s.logError(ctx, "ops_model_load_failed: "+err.Error())
		return modelView{}, apierr.New(apierr.CodeInternalError)
	}
	items := []modelView{view}
	if err := s.attachModelDetails(ctx, tx, items, []int64{id}); err != nil {
		s.logError(ctx, "ops_model_details_failed: "+err.Error())
		return modelView{}, apierr.New(apierr.CodeInternalError)
	}
	return items[0], nil
}

type dbQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func loadAliases(ctx context.Context, q dbQuerier, ids []int64) (map[int64][]string, error) {
	out := make(map[int64][]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `
		SELECT model_id, alias_name FROM t_model_alias
		 WHERE enabled = 1 AND model_id IN (`+placeholders(len(ids))+`)
		 ORDER BY alias_name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    int64
			alias string
		)
		if err := rows.Scan(&id, &alias); err != nil {
			return nil, err
		}
		out[id] = append(out[id], alias)
	}
	return out, rows.Err()
}

func loadCapabilities(ctx context.Context, q dbQuerier, ids []int64) (map[int64]map[string]bool, error) {
	out := make(map[int64]map[string]bool, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `
		SELECT model_id, cap_code, supported FROM t_model_capability
		 WHERE model_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id   int64
			code string
			on   bool
		)
		if err := rows.Scan(&id, &code, &on); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]bool, 4)
		}
		out[id][code] = on
	}
	return out, rows.Err()
}

// loadEffectivePrices mirrors boot.RefreshPrices: the highest version among
// enabled MODEL-scope rules inside their effective window. This segment only
// displays it; writing prices is the pricing segment's job.
func loadEffectivePrices(ctx context.Context, q dbQuerier, ids []int64, now time.Time) (map[int64]modelPricingView, error) {
	out := make(map[int64]modelPricingView, len(ids))
	args := make([]any, 0, len(ids)+4)
	args = append(args, now, now, now, now)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT p.scope_id, p.id, p.version, p.effective_from,
		       p.in_price_micro_per_k, p.out_price_micro_per_k, p.per_call_price_micro,
		       p.cache_hit_ratio_bp, p.cache_write_ratio_bp,
		       p.long_ctx_threshold_tokens, p.long_ctx_in_price_micro_per_k,
		       p.long_ctx_out_price_micro_per_k,
		       p.cost_in_micro_per_k, p.cost_out_micro_per_k, p.cost_per_call_micro
		  FROM t_price_rule p
		 WHERE p.scope_level = 'MODEL' AND p.enabled = 1
		   AND p.effective_from <= ? AND (p.effective_to IS NULL OR p.effective_to > ?)
		   AND p.id = (
		       SELECT p2.id FROM t_price_rule p2
		        WHERE p2.scope_level = 'MODEL' AND p2.scope_id = p.scope_id AND p2.enabled = 1
		          AND p2.effective_from <= ? AND (p2.effective_to IS NULL OR p2.effective_to > ?)
		        ORDER BY p2.version DESC, p2.id DESC LIMIT 1)
		   AND p.scope_id IN (`+placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			modelID              int64
			p                    modelPricingView
			effective            time.Time
			cacheHit, cacheWrite sql.NullInt64
			longThreshold        sql.NullInt64
			longIn, longOut      sql.NullInt64
			costIn, costOut      sql.NullInt64
			costPerCall          sql.NullInt64
		)
		if err := rows.Scan(&modelID, &p.RuleID, &p.Version, &effective,
			&p.InPriceMicroPerK, &p.OutPriceMicroPerK, &p.PerCallPriceMicro,
			&cacheHit, &cacheWrite, &longThreshold, &longIn, &longOut,
			&costIn, &costOut, &costPerCall); err != nil {
			return nil, err
		}
		p.EffectiveFrom = effective.UTC()
		p.CacheHitRatioBP = nullableInt64Ptr(cacheHit)
		p.CacheWriteRatioBP = nullableInt64Ptr(cacheWrite)
		p.LongCtxThresholdTokens = nullableInt64Ptr(longThreshold)
		p.LongCtxInPriceMicro = nullableInt64Ptr(longIn)
		p.LongCtxOutPriceMicro = nullableInt64Ptr(longOut)
		p.CostInMicroPerK = nullableInt64Ptr(costIn)
		p.CostOutMicroPerK = nullableInt64Ptr(costOut)
		p.CostPerCallMicro = nullableInt64Ptr(costPerCall)
		out[modelID] = p
	}
	return out, rows.Err()
}

func buildCapabilities(mask int64, configured map[string]bool) modelCapabilities {
	return modelCapabilities{
		Stream:   capValue("STREAM", 0, mask, configured),
		ToolCall: capValue("TOOL_CALL", 1, mask, configured),
		Vision:   capValue("VISION", 2, mask, configured),
		JSONMode: capValue("JSON_MODE", 3, mask, configured),
	}
}

func capValue(code string, bit uint, mask int64, configured map[string]bool) *bool {
	if v, ok := configured[code]; ok {
		return &v
	}
	if mask&(1<<bit) != 0 {
		v := true
		return &v
	}
	return nil
}

func replaceAliases(ctx context.Context, tx *sql.Tx, modelID int64, aliases []string, now time.Time) *apierr.APIError {
	if _, err := tx.ExecContext(ctx, `DELETE FROM t_model_alias WHERE model_id = ?`, modelID); err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	for _, alias := range aliases {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO t_model_alias (alias_name, model_id, source, enabled, created_at)
			VALUES (?,?, 'OPS', 1, ?)`, alias, modelID, now); err != nil {
			return apierr.New(apierr.CodeInternalError).WithCause(err)
		}
	}
	return nil
}

func replaceCapabilities(ctx context.Context, tx *sql.Tx, modelID int64, caps map[string]bool, now time.Time) *apierr.APIError {
	if _, err := tx.ExecContext(ctx, `DELETE FROM t_model_capability WHERE model_id = ?`, modelID); err != nil {
		return apierr.New(apierr.CodeInternalError).WithCause(err)
	}
	for key, on := range caps {
		code := strings.ToUpper(key)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO t_model_capability (model_id, cap_code, supported, updated_at)
			VALUES (?,?,?,?)`, modelID, code, on, now); err != nil {
			return apierr.New(apierr.CodeInternalError).WithCause(err)
		}
	}
	return nil
}

// insertAnnouncementDraft writes the DRAFT row that alerts customers to a
// listing-state change. It never touches the model state: the announcement is
// a notification, not a state machine driver (PRD F17-3).
func insertAnnouncementDraft(ctx context.Context, tx *sql.Tx, title, content string, operatorID int64, now time.Time) (int64, error) {
	title = truncateRunes(title, 128)
	content = truncateRunes(content, 2000)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_announcement
			(title, content, ann_type, position_mask, pinned, effective_from,
			 state, created_by, created_at, updated_at)
		VALUES (?,?, 'NOTICE', 2, 0, ?, 'DRAFT', ?, ?, ?)`,
		title, content, now, operatorID, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func listingAnnouncementCopy(m modelView, nextState string) (string, string) {
	switch nextState {
	case listingCatalogOnly:
		return m.DisplayName + " 调整为仅目录可见",
			fmt.Sprintf("模型 %s（%s）已调整为仅目录可见：控制台广场不再展示，路由侧不再选中该模型。", m.DisplayName, m.ModelName)
	case listingListed:
		return m.DisplayName + " 重新上架",
			fmt.Sprintf("模型 %s（%s）已重新上架，可在控制台广场查看并按分组价格调用。", m.DisplayName, m.ModelName)
	default:
		return m.DisplayName + " 状态变更", ""
	}
}

func modelAuditView(v modelView) map[string]any {
	return map[string]any{
		"model_name":        v.ModelName,
		"display_name":      v.DisplayName,
		"region":            v.Region,
		"model_type":        v.ModelType,
		"protocol_id":       v.ProtocolID,
		"primary_channel":   v.PrimaryChannel,
		"context_length":    v.ContextLength,
		"max_output_tokens": v.MaxOutputTokens,
		"capability_mask":   v.CapabilityMask,
		"listing_state":     v.ListingState,
		"recommend_weight":  v.RecommendWeight,
		"aliases":           v.Aliases,
		"published_at":      v.PublishedAt,
		"delisted_at":       v.DelistedAt,
	}
}

func modelOrder(page mw.Page) string {
	col := "m.created_at"
	switch page.Sort {
	case "updated_at":
		col = "m.updated_at"
	case "model_name":
		col = "m.model_name"
	case "recommend_weight":
		col = "m.recommend_weight"
	case "published_at":
		col = "m.published_at"
	}
	dir := orderDirection(page.Order)
	return col + " " + dir + ", m.id " + dir
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
