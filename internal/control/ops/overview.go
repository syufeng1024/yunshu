package ops

import (
	"context"
	"net/http"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
	"yunshu.dev/core/internal/control/ticket"
)

// The operator overview is a fixed set of real aggregates. It deliberately
// has no hourly series or seven-day estimate: "today" means the CST calendar
// day, and a missing denominator yields null rather than zero.
var overviewCST = time.FixedZone("CST", 8*3600)

// reconDiffWindow bounds the open-difference alert so the query can use
// idx_rd_date_type (biz_date, diff_type, status). Counting every historical
// OPEN row would need a status-first index, which does not exist.
const reconDiffWindowDays = 30

func startOfOverviewDay(t time.Time) time.Time {
	local := t.In(overviewCST)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, overviewCST)
}

type adminOverviewToday struct {
	Requests      int64 `json:"requests"`
	ConsumedMicro int64 `json:"consumed_micro"`
	// nil when the internal availability denominator is zero. Never 0 to
	// impersonate "no data" (hard constraint 2).
	SuccessRateBP *int64 `json:"success_rate_bp"`
}

type adminOverviewAlerts struct {
	DanglingPrehold int64 `json:"dangling_prehold"`
	ReconDiff       int64 `json:"recon_diff"`
	SLAOverdue      int64 `json:"sla_overdue"`
	ChannelAbnormal int64 `json:"channel_abnormal"`
}

type adminOverviewCounts struct {
	PendingReviews int64 `json:"pending_reviews"`
	OpenTickets    int64 `json:"open_tickets"`
}

type adminOverviewResponse struct {
	Today  adminOverviewToday  `json:"today"`
	Alerts adminOverviewAlerts `json:"alerts"`
	Counts adminOverviewCounts `json:"counts"`
}

// Overview implements GET /api/admin/overview (permission admin.read).
//
// Today's counting rule intentionally mirrors the console overview
// (internal/control/user/overview.go): request_total counts every row,
// consumed_micro sums every row (failed rows are zero), and the internal
// availability denominator excludes 402/429 business rejections. The rate is
// null when that denominator is zero. consumed_micro excludes the internal
// group (T31 D-28 ③): those rows are not billed and are not revenue.
//
// open_tickets means unresolved (OPEN or RESPONDED). sla_overdue is the same
// first-response predicate as the ticket alert zone.
func (s *Service) Overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now()
	dayStart := startOfOverviewDay(now)

	var (
		requests, consumed, successes, denominator int64
	)
	// T31 D-28 ③: the internal group is not billed, so its rows must not
	// count as revenue. Request count and the availability denominator keep
	// every row; only the money sum is scoped to billable groups.
	err := s.DB.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       CAST(COALESCE(SUM(CASE WHEN group_code_snap <> 'internal'
		                              THEN consumed_micro ELSE 0 END), 0) AS SIGNED),
		       CAST(COALESCE(SUM(CASE WHEN result = 'SUCCESS' THEN 1 ELSE 0 END), 0) AS SIGNED),
		       CAST(COALESCE(SUM(CASE WHEN http_status NOT IN (402, 429) THEN 1 ELSE 0 END), 0) AS SIGNED)
		  FROM t_usage_record
		 WHERE created_at >= ? AND created_at < ?`,
		dayStart.UTC(), dayStart.Add(24*time.Hour).UTC()).Scan(
		&requests, &consumed, &successes, &denominator)
	if err != nil {
		s.logError(ctx, "admin_overview_today_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	resp := adminOverviewResponse{
		Today: adminOverviewToday{
			Requests:      requests,
			ConsumedMicro: consumed,
		},
	}
	if denominator > 0 {
		bp := successes * 10000 / denominator
		resp.Today.SuccessRateBP = &bp
	}

	dangling, apiErr := s.overviewCount(ctx,
		`SELECT COUNT(*) FROM t_prehold_hold WHERE state = 'HELD' AND expires_at < ?`, now)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp.Alerts.DanglingPrehold = dangling

	bizDateFloor := now.In(overviewCST).AddDate(0, 0, -reconDiffWindowDays).Format("2006-01-02")
	bizDateCeiling := now.In(overviewCST).Format("2006-01-02")
	recon, apiErr := s.overviewCount(ctx, `
		SELECT COUNT(*) FROM t_reconcile_diff
		 WHERE status = 'OPEN' AND biz_date >= ? AND biz_date <= ?`,
		bizDateFloor, bizDateCeiling)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp.Alerts.ReconDiff = recon

	// sla_overdue is the same predicate jobsd exports as gw_ticket_sla_overdue
	// (G25). It lives in the ticket package so the operator page and the alert
	// cannot drift: one statement, two consumers.
	slaOverdue, err := ticket.CountFirstResponseOverdue(ctx, s.DB, now)
	if err != nil {
		s.logError(ctx, "admin_overview_sla_overdue_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	resp.Alerts.SLAOverdue = slaOverdue

	// V1.0 is domestic-only; the region predicate lets this use
	// idx_channel_region_enabled instead of scanning the configuration table.
	channelAbnormal, apiErr := s.overviewCount(ctx, `
		SELECT COUNT(*) FROM t_channel
		 WHERE deleted_at IS NULL AND region = 'DOMESTIC' AND enabled = 1
		   AND health_state <> 'CLOSED'`)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp.Alerts.ChannelAbnormal = channelAbnormal

	pendingReviews, apiErr := s.overviewCount(ctx,
		`SELECT COUNT(*) FROM t_review_task WHERE state = 'PENDING'`)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp.Counts.PendingReviews = pendingReviews

	openTickets, apiErr := s.overviewCount(ctx,
		`SELECT COUNT(*) FROM t_ticket WHERE state IN ('OPEN', 'RESPONDED')`)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	resp.Counts.OpenTickets = openTickets

	_ = mw.WriteJSON(w, http.StatusOK, resp)
}

func (s *Service) overviewCount(ctx context.Context, query string, args ...any) (int64, *apierr.APIError) {
	var count int64
	if err := s.DB.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		s.logError(ctx, "admin_overview_count_failed: "+err.Error())
		return 0, apierr.New(apierr.CodeInternalError)
	}
	return count, nil
}
