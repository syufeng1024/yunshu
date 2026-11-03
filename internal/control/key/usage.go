package key

import (
	"net/http"
	"strconv"
	"time"

	"yunshu.dev/core/internal/apierr"
	"yunshu.dev/core/internal/control/mw"
)

// 详情页的趋势只回答「这把密钥最近用得怎么样」，更长的区间应该去用量页看账单。
const (
	keyUsageDefaultHours = 24
	keyUsageMaxHours     = 168 // 7 天
)

// 用量按东八区自然小时分桶，与账户概览的 hourlySeries 保持一致：同一个账号在
// 两个页面上看到的小时边界不能不一样。
var cstZone = time.FixedZone("CST", 8*3600)

type keyUsagePoint struct {
	Hour          string `json:"hour"`
	Requests      int64  `json:"requests"`
	ConsumedMicro int64  `json:"consumed_micro"`
}

type keyUsageResponse struct {
	KeyID              int64           `json:"key_id"`
	Hours              int             `json:"hours"`
	Points             []keyUsagePoint `json:"points"`
	TotalRequests      int64           `json:"total_requests"`
	TotalConsumedMicro int64           `json:"total_consumed_micro"`
}

// Usage implements GET /api/console/keys/{id}/usage?hours=24.
//
// 账户级概览只给全账号的总量，用户看不出某把密钥是「主力」还是「早就没人用」。
// 这张图是决定轮换或停用一把密钥时唯一能看的依据。
//
// 空桶补零：图表需要连续的时间点，少一个小时会把曲线画成一条跨过空档的直线。
func (s *Service) Usage(w http.ResponseWriter, r *http.Request) {
	sess, ok := mw.SessionFrom(r.Context())
	if !ok {
		mw.WriteError(w, r, mw.SessionMissing())
		return
	}
	id, apiErr := pathKeyID(r)
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	// 归属校验放在查询之前：没有这一步，改 URL 里的 id 就能读到别人的用量。
	if _, apiErr = s.loadOwnedKey(r.Context(), s.DB, sess.UID, id, false); apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}

	hours := keyUsageDefaultHours
	if raw := r.URL.Query().Get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > keyUsageMaxHours {
			e := mw.InvalidField("hours")
			mw.WriteError(w, r, e.WithHint("hours 需在 1 到 168 之间"))
			return
		}
		hours = n
	}

	ctx := r.Context()
	endHour := s.now().In(cstZone).Truncate(time.Hour)
	start := endHour.Add(-time.Duration(hours-1) * time.Hour)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT DATE_FORMAT(CONVERT_TZ(created_at, '+00:00', '+08:00'), '%Y-%m-%d %H:00:00'),
		       COUNT(*), COALESCE(SUM(consumed_micro), 0)
		  FROM t_usage_record
		 WHERE api_key_id = ? AND created_at >= ? AND created_at < ?
		 GROUP BY 1`,
		id, start.UTC(), endHour.Add(time.Hour).UTC())
	if err != nil {
		s.logError(ctx, "key_usage_series_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	type agg struct {
		requests int64
		consumed int64
	}
	byHour := make(map[string]agg, hours)
	for rows.Next() {
		var (
			hour string
			a    agg
		)
		if err := rows.Scan(&hour, &a.requests, &a.consumed); err != nil {
			s.logError(ctx, "key_usage_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		byHour[hour] = a
	}
	if err := rows.Err(); err != nil {
		s.logError(ctx, "key_usage_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	points := make([]keyUsagePoint, 0, hours)
	var totalReq, totalConsumed int64
	for i := 0; i < hours; i++ {
		hour := start.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05")
		a := byHour[hour]
		points = append(points, keyUsagePoint{Hour: hour, Requests: a.requests, ConsumedMicro: a.consumed})
		totalReq += a.requests
		totalConsumed += a.consumed
	}

	_ = mw.WriteJSON(w, http.StatusOK, keyUsageResponse{
		KeyID:              id,
		Hours:              hours,
		Points:             points,
		TotalRequests:      totalReq,
		TotalConsumedMicro: totalConsumed,
	})
}
