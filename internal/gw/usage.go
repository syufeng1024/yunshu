package gw

import (
	"strings"
	"time"

	"yunshu.dev/core/internal/apierr"
)

// FailureUsage is the failure-row subset of settle.UsageRecord. It deliberately
// carries no money or token columns: a request that failed before the upstream
// terminal ran has no prehold, consumption or refund, so the type cannot
// express a non-zero amount. The adaptation into the full P28 row (which needs
// the money columns zeroed explicitly) lives in cmd/gatewayd to avoid an import
// cycle from this package into internal/settle.
type FailureUsage struct {
	RequestID     string
	ConfigVersion int64
	UserID        int64
	APIKeyID      int64
	APIKeyName    string
	ModelID       int64
	ModelName     string
	RequestModel  string
	InboundProto  string
	PriceRuleID   int64
	GroupCode     string
	PriceRatioBP  int64
	IsStream      bool
	HTTPStatus    int
	BizCode       string
	Result        string
	FailStage     string // T-35 word, normalised through stageForFail
	FailOwner     string // CLIENT / PLATFORM / UPSTREAM
	TotalMS       int64
	ClientIP      []byte
	UserAgent     string
	CreatedAt     time.Time
	RawBody       []byte
}

// emitFailureUsage backfills one usage row for a request that failed before the
// upstream terminal ran (P01-P14). The dispatcher already writes its own row
// from a deferred function in Serve, so terminalRan suppresses the backfill to
// keep exactly one row per request.
//
// fail_stage in t_usage_record is a single T-35 vocabulary: pipeline failures
// are normalised here through stageForFail, while the dispatcher writes T-35
// words directly (see internal/dispatch/usagerecord.go attribution()).
func emitFailureUsage(h *Handler, c *Ctx, terminalRan bool) {
	if h == nil || h.d == nil || h.d.FailureSink == nil {
		return
	}
	if terminalRan {
		return
	}
	if c == nil || c.Auth == nil {
		return
	}
	if c.Endpoint == EndpointModels {
		return
	}
	if c.ErrCode == "" || c.ErrCode == "connection_reset" {
		return
	}

	status := 500
	if spec, ok := apierr.Lookup(c.ErrCode); ok && spec.HTTPStatus != 0 {
		status = spec.HTTPStatus
	}

	modelID, modelName, requestModel := int64(0), "", ""
	if c.Model != nil {
		modelID = c.Model.ID
		modelName = c.Model.ModelName
	}
	if c.IR != nil {
		requestModel = c.IR.Model
	}
	if requestModel == "" {
		requestModel = modelName
	}

	priceRuleID := int64(0)
	if c.Price != nil {
		priceRuleID = c.Price.ID
	}
	groupCode := "standard"
	if c.Auth.GroupCode != "" {
		groupCode = c.Auth.GroupCode
	}
	priceRatioBP := int64(10000)
	if c.Auth.GroupRatioBP > 0 {
		priceRatioBP = c.Auth.GroupRatioBP
	}
	var clientIP []byte
	if c.ClientIP.IsValid() {
		clientIP = c.ClientIP.AsSlice()
	}
	userAgent := ""
	if c.Header != nil {
		userAgent = c.Header.Get("User-Agent")
	}

	h.d.FailureSink(FailureUsage{
		RequestID:     c.RequestID,
		ConfigVersion: c.ConfigVersion,
		UserID:        c.Auth.UserID,
		APIKeyID:      c.Auth.KeyID,
		APIKeyName:    c.Auth.KeyName,
		ModelID:       modelID,
		ModelName:     modelName,
		RequestModel:  requestModel,
		InboundProto:  string(c.Protocol),
		PriceRuleID:   priceRuleID,
		GroupCode:     groupCode,
		PriceRatioBP:  priceRatioBP,
		IsStream:      c.Stream,
		HTTPStatus:    status,
		BizCode:       c.ErrCode,
		Result:        "FAILED",
		FailStage:     stageForFail(c.FailStage),
		FailOwner:     ownerForCode(c.ErrCode),
		TotalMS:       time.Since(c.StartedAt).Milliseconds(),
		ClientIP:      clientIP,
		UserAgent:     userAgent,
		CreatedAt:     c.StartedAt,
		RawBody:       c.RawBody,
	})
}

// ownerForCode maps an API error code to the fail_owner vocabulary shared with
// the dispatcher. BUSINESS means the client owns the failure; UPSTREAM means
// the channel does; everything else (including an unknown code) is PLATFORM.
func ownerForCode(code string) string {
	spec, ok := apierr.Lookup(code)
	if !ok {
		return "PLATFORM"
	}
	switch spec.Owner {
	case apierr.OwnerBusiness:
		return "CLIENT"
	case apierr.OwnerUpstream:
		return "UPSTREAM"
	default:
		return "PLATFORM"
	}
}

// stageForFail normalises a pipeline P-number into the same T-35 vocabulary the
// dispatcher writes (see usagerecord.go attribution()). The inbound pipeline
// only ever sets P01-P13 plus the P24 client-abort sentinel before the terminal
// runs; the P14-P31 rows are mapped defensively so no P-number is silently
// dropped. An empty or unknown value returns "" and the column is left NULL.
func stageForFail(failStage string) string {
	switch strings.TrimSpace(failStage) {
	// P01-P04: connection, body read, endpoint match, request parse.
	case "P01", "P02", "P03", "P04":
		return "REQUEST"
	// P05-P08.5: authentication and the authorization gates that follow it.
	case "P05", "P06", "P06.5", "P07", "P08", "P08.5":
		return "AUTH"
	// P09/P10: quota pre-check and prehold, the money gates.
	case "P09", "P10":
		return "SETTLE"
	// P11: the four-dimension rate gate.
	case "P11":
		return "RATE_LIMIT"
	// P12/P13: route selection and breaker filtering.
	case "P12", "P13":
		return "ROUTE"
	// P14-P23, P25: the upstream half (conversion, call, stream, usage
	// extraction), matching attribution()'s default bucket. StageOf folds P14
	// and P20 into the same upstream group rather than a separate CONVERT word.
	case "P14", "P15", "P16", "P17", "P18", "P19", "P20", "P21", "P22", "P23", "P25":
		return "UPSTREAM"
	// P24: client disconnect detection. StageOf groups it with upstream, but
	// the pipeline raises CodeClientAborted here, so the row must use
	// CLIENT_ABORT to match attribution()'s clientAborted branch.
	case "P24":
		return "CLIENT_ABORT"
	// P26-P29: the settlement tail (enqueue, release, usage/attempt logs).
	case "P26", "P27", "P28", "P29":
		return "SETTLE"
	// P30/P31 and unknown values have no failure semantics in StageOf; leave
	// the column NULL rather than fabricate a word.
	default:
		return ""
	}
}
