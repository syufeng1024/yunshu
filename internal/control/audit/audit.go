// Package audit writes the append-only control-plane trail.
//
// The first segment records the minimum columns needed to answer "who did
// what, when, to which object, and what did it look like before/after". The
// second segment extends before/after to the full object and adds a reader
// endpoint; the table and writer shape do not change.
package audit

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"yunshu.dev/core/internal/jsonx"
	obslog "yunshu.dev/core/internal/obs/log"
	"yunshu.dev/core/internal/platform/clock"
)

// Execer is satisfied by *sql.DB and *sql.Tx. Register and key creation pass a
// transaction so the audit row and the state change commit together: an
// unlogged write cannot be committed.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Entry is one t_admin_audit_log row.
type Entry struct {
	OperatorID   int64
	OperatorRole string
	OperatorIP   []byte
	Module       string
	Action       string
	TargetType   string
	TargetID     string
	Before       []byte
	After        []byte
	Reason       string
	Result       string
}

// Writer binds the audit sink to a clock.
type Writer struct {
	DB  *sql.DB
	Now clock.Clock
}

func (w *Writer) now() clock.Clock {
	if w.Now != nil {
		return w.Now
	}
	return clock.Real{}
}

// Insert writes one row. It never stores passwords, key plaintext or upstream
// keys: callers must mask before building Before/After.
func (w *Writer) Insert(ctx context.Context, q Execer, e Entry) error {
	if q == nil {
		return fmt.Errorf("audit: executor is nil")
	}
	if e.OperatorID <= 0 {
		// System-initiated seed writes use operator 0; the column is NOT NULL.
		e.OperatorID = 0
	}
	if e.Result == "" {
		e.Result = "OK"
	}
	if len(e.OperatorIP) > 16 {
		e.OperatorIP = e.OperatorIP[:16]
	}
	meta := obslog.MetaFrom(ctx)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = meta.RequestID
	}
	if traceID == "" {
		traceID = "seed"
	}
	const qry = `INSERT INTO t_admin_audit_log
		(trace_id, operator_id, operator_role, operator_ip, module, action,
		 target_type, target_id, before_json, after_json, reason, result, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`
	_, err := q.ExecContext(ctx, qry,
		traceID, e.OperatorID, e.OperatorRole, nullableBytes(e.OperatorIP),
		e.Module, e.Action, e.TargetType, e.TargetID,
		nullableBytes(e.Before), nullableBytes(e.After),
		nullableString(e.Reason), e.Result, w.now().Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("audit: insert t_admin_audit_log: %w", err)
	}
	return nil
}

// IPBytes converts the request's peer address to the varbinary(16) form.
// Proxies are not trusted here in the first segment: no trusted-proxy list has
// been defined yet, so X-Forwarded-For would be attacker-controlled.
// IPBytes 取请求来源地址。
//
// 只信任**来自私有/回环网段**的 X-Real-IP：本项目的标准部署是 nginx 转发到
// controld，nginx 用 `$remote_addr` 覆盖写这个头，所以在容器网络里它是真实
// 客户端地址，且客户端无法伪造。反过来，如果 controld 的端口被直接暴露到
// 公网，直连请求的 RemoteAddr 是公网地址，此时**不以**该头为准——否则任何人
// 只要自带一个 X-Real-IP 就能绕过登录失败锁定与验证码的按 IP 配额。
//
// 不使用 X-Forwarded-For：nginx 用 $proxy_add_x_forwarded_for 追加，其第一个
// 值是客户端自己填的，用它等于把限流键交给攻击者。
func IPBytes(r *http.Request) []byte {
	if r == nil {
		return nil
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil || !addr.IsValid() {
		return nil
	}
	if addr.IsPrivate() || addr.IsLoopback() {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			if forwarded, perr := netip.ParseAddr(v); perr == nil && forwarded.IsValid() {
				return forwarded.Unmap().AsSlice()
			}
		}
	}
	return addr.AsSlice()
}

// MarshalBeforeAfter is a convenience for callers that already have structs.
func MarshalBeforeAfter(before, after any) ([]byte, []byte, error) {
	var b, a []byte
	if before != nil {
		raw, err := jsonx.Marshal(before)
		if err != nil {
			return nil, nil, err
		}
		b = raw
	}
	if after != nil {
		raw, err := jsonx.Marshal(after)
		if err != nil {
			return nil, nil, err
		}
		a = raw
	}
	return b, a, nil
}

func nullableBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}

func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}
