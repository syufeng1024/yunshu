package ticket

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

// slaView is the answer to "已响应 / 剩余 xx". Everything is integer seconds;
// the display string is built server-side so the console and the operator
// queue cannot disagree about what "remaining" means.
type slaView struct {
	FirstResponseDueAt time.Time  `json:"first_response_due_at"`
	FirstRespondedAt   *time.Time `json:"first_responded_at"`
	Responded          bool       `json:"responded"`
	Overdue            bool       `json:"overdue"`
	RemainingSeconds   int64      `json:"remaining_seconds"`
	OverdueSeconds     int64      `json:"overdue_seconds"`
	Display            string     `json:"display"`
}

// buildSLA computes the 4-hour first-response countdown.
//
// It reads the clock, never writes state. Overdue is a display fact: the
// ticket stays OPEN, no reply is inserted, no status is escalated, and nothing
// closes it (PRD F21-2). The only place overdue changes a query is the
// operator alert filter.
//
// A ticket closed without a reply has no stopwatch left to show. Rendering
// "剩余" or "已超时" for it would claim the platform still owes an answer,
// while the alert query correctly excludes it; both views must agree.
func buildSLA(created, due time.Time, respondedAt *time.Time, state string, now time.Time) slaView {
	out := slaView{FirstResponseDueAt: due, FirstRespondedAt: respondedAt}
	if respondedAt != nil {
		used := respondedAt.Sub(created)
		if used < 0 {
			used = 0
		}
		out.Responded = true
		out.Display = "已响应（用时 " + humanDuration(used) + "）"
		return out
	}
	if state == StateClosed {
		out.Display = "已关闭（未响应）"
		return out
	}
	if now.Before(due) {
		remaining := due.Sub(now)
		out.RemainingSeconds = ceilSeconds(remaining)
		out.Display = "剩余 " + humanDuration(remaining)
		return out
	}
	overdue := now.Sub(due)
	out.Overdue = true
	out.OverdueSeconds = int64(overdue / time.Second)
	out.Display = "已超时 " + humanDuration(overdue)
	return out
}

// ceilSeconds rounds up so a 3h59m59.5s remainder does not render as 3h59m59s
// while the ticket is still within its window.
func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	secs := int64(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// humanDuration renders whole seconds as Chinese hours/minutes/seconds. No
// float arithmetic is involved.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d 小时 %d 分钟", h, m)
	case h > 0:
		return fmt.Sprintf("%d 小时", h)
	case m > 0 && s > 0:
		return fmt.Sprintf("%d 分钟 %d 秒", m, s)
	case m > 0:
		return fmt.Sprintf("%d 分钟", m)
	default:
		return fmt.Sprintf("%d 秒", s)
	}
}

// ---------------------------------------------------------------------------
// GET /api/admin/tickets — the operator queue and the SLA alert zone
// ---------------------------------------------------------------------------

type adminTicketRow struct {
	ID                 int64
	TicketNo           string
	UserID             int64
	UserEmail          string
	TicketType         string
	Title              string
	RequestID          sql.NullString
	PolicyHitID        sql.NullInt64
	State              string
	FirstResponseDueAt time.Time
	FirstRespondedAt   sql.NullTime
	LastRepliedAt      sql.NullTime
	CreatedAt          time.Time
}

type adminTicketItem struct {
	ID                 int64      `json:"id"`
	TicketNo           string     `json:"ticket_no"`
	UserID             int64      `json:"user_id"`
	UserEmail          string     `json:"user_email"`
	TicketType         string     `json:"ticket_type"`
	Title              string     `json:"title"`
	RequestID          *string    `json:"request_id"`
	PolicyHitID        *int64     `json:"policy_hit_id"`
	State              string     `json:"state"`
	FirstResponseDueAt time.Time  `json:"first_response_due_at"`
	FirstRespondedAt   *time.Time `json:"first_responded_at"`
	LastRepliedAt      *time.Time `json:"last_replied_at"`
	CreatedAt          time.Time  `json:"created_at"`
	SLA                slaView    `json:"sla"`
}

type adminTicketListResponse struct {
	Items    []adminTicketItem `json:"items"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
	Total    int64             `json:"total"`
}

// parseOverdue accepts "", all, true/1 and false/0. An unknown value is a
// query error rather than "no filter", because a silent fallback would let the
// alert view render the whole queue and look calm.
func parseOverdue(raw string) (*bool, *apierr.APIError) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "all":
		return nil, nil
	case "true", "1":
		v := true
		return &v, nil
	case "false", "0":
		v := false
		return &v, nil
	default:
		return nil, mw.InvalidQueryField("overdue")
	}
}

// ListAdmin implements GET /api/admin/tickets. With overdue=true it returns
// the SLA alert zone ordered by how long each ticket has been overdue.
//
// This is a read: listing an overdue ticket never mutates it.
func (s *Service) ListAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	page, apiErr := mw.ParsePage(r, "created_at", "first_response_due_at")
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	state, apiErr := parseStateFilter(r.URL.Query().Get("state"))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	typ, apiErr := parseTypeFilter(r.URL.Query().Get("type"))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	overdue, apiErr := parseOverdue(r.URL.Query().Get("overdue"))
	if apiErr != nil {
		mw.WriteError(w, r, apiErr)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) > maxSearchRunes {
		mw.WriteError(w, r, mw.InvalidQueryField("q"))
		return
	}

	now := s.now()
	where := []string{"1 = 1"}
	args := []any{}
	if state != "" {
		where = append(where, "t.state = ?")
		args = append(args, state)
	}
	if typ != "" {
		where = append(where, "t.ticket_type = ?")
		args = append(args, typ)
	}
	if overdue != nil {
		// The alert predicate: still waiting for a first reply and past the
		// due instant. RESPONDED and CLOSED rows can never be in the zone.
		pred := "(t.state = ? AND t.first_responded_at IS NULL AND t.first_response_due_at < ?)"
		args = append(args, StateOpen, now)
		if *overdue {
			where = append(where, pred)
		} else {
			where = append(where, "NOT "+pred)
		}
	}
	if q != "" {
		where = append(where, `(t.ticket_no LIKE ? ESCAPE '!' OR t.title LIKE ? ESCAPE '!')`)
		pat := likePattern(q)
		args = append(args, pat, pat)
	}
	whereSQL := strings.Join(where, " AND ")

	var total int64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_ticket t WHERE `+whereSQL, args...).Scan(&total); err != nil {
		s.logError(ctx, "ticket_admin_count_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}

	// Default order: newest first, except in the alert zone, where the
	// longest-overdue ticket comes first so the on-call operator starts at the
	// top (PRD §5.9 「按超时时长排序供值班人认领」).
	sortCol := "t.created_at"
	direction := "DESC"
	switch page.Sort {
	case "first_response_due_at":
		sortCol = "t.first_response_due_at"
		direction = strings.ToUpper(page.Order)
	case "created_at":
		direction = strings.ToUpper(page.Order)
	default:
		if overdue != nil && *overdue {
			sortCol = "t.first_response_due_at"
			direction = "ASC"
		}
	}

	queryArgs := append(append([]any{}, args...), page.Size, page.OffsetInt64())
	rows, err := s.DB.QueryContext(ctx, `
		SELECT t.id, t.ticket_no, t.user_id, COALESCE(u.email, ''),
		       t.ticket_type, t.title, t.request_id, t.policy_hit_id, t.state,
		       t.first_response_due_at, t.first_responded_at, t.last_replied_at,
		       t.created_at
		  FROM t_ticket t
		  LEFT JOIN t_user u ON u.id = t.user_id
		 WHERE `+whereSQL+`
		 ORDER BY `+sortCol+` `+direction+`, t.id `+direction+`
		 LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		s.logError(ctx, "ticket_admin_list_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	defer rows.Close()

	items := make([]adminTicketItem, 0, page.Size)
	for rows.Next() {
		var row adminTicketRow
		if err := rows.Scan(
			&row.ID, &row.TicketNo, &row.UserID, &row.UserEmail,
			&row.TicketType, &row.Title, &row.RequestID, &row.PolicyHitID,
			&row.State, &row.FirstResponseDueAt, &row.FirstRespondedAt,
			&row.LastRepliedAt, &row.CreatedAt,
		); err != nil {
			s.logError(ctx, "ticket_admin_scan_failed: "+err.Error())
			mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
			return
		}
		row.FirstResponseDueAt = row.FirstResponseDueAt.UTC()
		row.CreatedAt = row.CreatedAt.UTC()
		respondedAt := nullTimePtr(row.FirstRespondedAt)
		items = append(items, adminTicketItem{
			ID:                 row.ID,
			TicketNo:           row.TicketNo,
			UserID:             row.UserID,
			UserEmail:          row.UserEmail,
			TicketType:         row.TicketType,
			Title:              row.Title,
			RequestID:          nullStringPtr(row.RequestID),
			PolicyHitID:        nullInt64Ptr(row.PolicyHitID),
			State:              row.State,
			FirstResponseDueAt: row.FirstResponseDueAt,
			FirstRespondedAt:   respondedAt,
			LastRepliedAt:      nullTimePtr(row.LastRepliedAt),
			CreatedAt:          row.CreatedAt,
			SLA:                buildSLA(row.CreatedAt, row.FirstResponseDueAt, respondedAt, row.State, now),
		})
	}
	if err := rows.Err(); err != nil {
		s.logError(ctx, "ticket_admin_rows_failed: "+err.Error())
		mw.WriteError(w, r, apierr.New(apierr.CodeInternalError))
		return
	}
	_ = mw.WriteJSON(w, http.StatusOK, adminTicketListResponse{
		Items: items, Page: page.Number, PageSize: page.Size, Total: total,
	})
}

// adminTicketByID renders one row for a mutation response.
func (s *Service) adminTicketByID(ctx context.Context, id int64) (*adminTicketItem, *apierr.APIError) {
	row, apiErr := s.loadAdminTicket(ctx, id)
	if apiErr != nil {
		return nil, apiErr
	}
	respondedAt := nullTimePtr(row.FirstRespondedAt)
	item := &adminTicketItem{
		ID:                 row.ID,
		TicketNo:           row.TicketNo,
		UserID:             row.UserID,
		TicketType:         row.TicketType,
		Title:              row.Title,
		RequestID:          nullStringPtr(row.RequestID),
		PolicyHitID:        nullInt64Ptr(row.PolicyHitID),
		State:              row.State,
		FirstResponseDueAt: row.FirstResponseDueAt,
		FirstRespondedAt:   respondedAt,
		LastRepliedAt:      nullTimePtr(row.LastRepliedAt),
		CreatedAt:          row.CreatedAt,
		SLA:                buildSLA(row.CreatedAt, row.FirstResponseDueAt, respondedAt, row.State, s.now()),
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(email, '') FROM t_user WHERE id = ?`,
		row.UserID).Scan(&item.UserEmail); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.logWarn(ctx, "ticket_admin_user_email_failed: "+err.Error())
	}
	return item, nil
}

// FirstResponseOverdueSQL counts the tickets in the first-response alert zone:
// still OPEN, nobody has answered yet, and the 4-hour deadline has passed
// (PRD §9.2 / F21-2). It is the ONE definition of that predicate as a count.
//
// Two consumers read it and they must never disagree, because one of them is
// what the platform promises and the other is how the platform notices:
//   - GET /api/admin/overview returns it as alerts.sla_overdue (operator page);
//   - jobsd exports it as gw_ticket_sla_overdue for TicketSLAFirstResponseOverdue
//     (G25), so a breach is an alert instead of a twice-an-hour manual poll.
//
// The state literal is 'OPEN' rather than a bind parameter so the statement
// stays a single countable string; TestFirstResponseOverdueSQLMatchesState
// keeps it equal to StateOpen.
const FirstResponseOverdueSQL = `SELECT COUNT(*) FROM t_ticket
		 WHERE state = 'OPEN' AND first_responded_at IS NULL AND first_response_due_at < ?`

// CountFirstResponseOverdue runs FirstResponseOverdueSQL. now is the instant
// the caller considers "now" (the services pass their injectable clock);
// passing a different instant here than the response uses would make the alert
// and the page disagree by however long the request ran.
func CountFirstResponseOverdue(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, nil
	}
	var n int64
	if err := db.QueryRowContext(ctx, FirstResponseOverdueSQL, now).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
