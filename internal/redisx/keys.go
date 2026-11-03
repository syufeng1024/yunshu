// Key constructors for TDS §2.1 K1–K30.
//
// Every key in this module is built here. A rate-limit key assembled with
// string concatenation anywhere else is a review reject (T06 hard constraint
// 19), because two spellings of the same key silently split one counter into
// two and double the allowance.
//
// Naming rules: lowercase, colon separated. {min_epoch} is a UTC minute
// ordinal (floor(unix_seconds/60)); {yyyymmdd_cst} is an Asia/Shanghai
// calendar day. The two never share a key, so a timezone change cannot move a
// per-minute counter into a daily one.
package redisx

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// MinEpochFromMS converts a corrected unix-millisecond timestamp to the
// per-minute bucket ordinal used by K1/K2.
func MinEpochFromMS(ms int64) int64 { return ms / 60_000 }

// K1 rl:rpm:{key_id}:{min_epoch}
func RPMLKey(keyID, minEpoch int64) string {
	return "rl:rpm:" + strconv.FormatInt(keyID, 10) + ":" + strconv.FormatInt(minEpoch, 10)
}

// K2 rl:tpm:{key_id}:{min_epoch}
func TPMKey(keyID, minEpoch int64) string {
	return "rl:tpm:" + strconv.FormatInt(keyID, 10) + ":" + strconv.FormatInt(minEpoch, 10)
}

// K3 rl:cc:{key_id}
func ConcurrencyKey(keyID int64) string {
	return "rl:cc:" + strconv.FormatInt(keyID, 10)
}

// K4 rl:dq:{user_id}:{yyyymmdd_cst}
func DailyQuotaKey(userID int64, day string) string {
	return "rl:dq:" + strconv.FormatInt(userID, 10) + ":" + day
}

// K5 rl:dqr:{user_id}:{yyyymmdd_cst}
func DailyQuotaRejectedKey(userID int64, day string) string {
	return "rl:dqr:" + strconv.FormatInt(userID, 10) + ":" + day
}

// K6 idem:{user_id}:{sha1(idem_key)}
//
// The hash argument must come from IdemHash, which folds scope and
// api_key_id into the digest. §1.6 requires (scope, api_key_id, idem_key) so
// one Key cannot read another Key's replay body.
func IdemKey(userID int64, hash string) string {
	return "idem:" + strconv.FormatInt(userID, 10) + ":" + hash
}

// K7 idem:lock:{user_id}:{sha1(idem_key)}
func IdemLockKey(userID int64, hash string) string {
	return "idem:lock:" + strconv.FormatInt(userID, 10) + ":" + hash
}

// K8 cfg:ver
func ConfigVersionKey() string { return "cfg:ver" }

// K9 cfg:snap:{namespace}
func ConfigSnapshotKey(namespace string) string { return "cfg:snap:" + namespace }

// K10 cfg:invalidate (Pub/Sub channel)
func ConfigInvalidateChannel() string { return "cfg:invalidate" }

// K11 hold:{request_id}
func HoldKey(requestID string) string { return "hold:" + requestID }

// K12 hold:recover:{request_id}
func HoldRecoverKey(requestID string) string { return "hold:recover:" + requestID }

// K13 ak:{sha256_prefix_16}
func APIKeyHashKey(sha256Prefix16 string) string { return "ak:" + sha256Prefix16 }

// K14 m:{scope_type}:{scope_id}:{min_epoch}
func MetricKey(scopeType string, scopeID, minEpoch int64) string {
	return "m:" + scopeType + ":" + strconv.FormatInt(scopeID, 10) + ":" + strconv.FormatInt(minEpoch, 10)
}

// K15 m:chan:{channel_id}:{min_epoch}
func ChannelMetricKey(channelID, minEpoch int64) string {
	return "m:chan:" + strconv.FormatInt(channelID, 10) + ":" + strconv.FormatInt(minEpoch, 10)
}

// K16 up:cb:{channel_id}
func BreakerKey(channelID int64) string {
	return "up:cb:" + strconv.FormatInt(channelID, 10)
}

// K17 up:probe:{channel_id}
func ProbeKey(channelID int64) string {
	return "up:probe:" + strconv.FormatInt(channelID, 10)
}

// K18 node:set
func NodeSetKey() string { return "node:set" }

// K19 price:{model_id}
func PriceKey(modelID int64) string {
	return "price:" + strconv.FormatInt(modelID, 10)
}

// K20 sess:admin:{session_id}
func AdminSessionKey(sessionID string) string { return "sess:admin:" + sessionID }

// K21 lock:recon:{yyyymmdd}
func ReconLockKey(day string) string { return "lock:recon:" + day }

// K22 lock:expire_batch
func ExpireBatchLockKey() string { return "lock:expire_batch" }

// K23 up:cd:{channel_id}
func CooldownKey(channelID int64) string {
	return "up:cd:" + strconv.FormatInt(channelID, 10)
}

// K24 log:stream
func LogStreamKey() string { return "log:stream" }

// K25 job:heartbeat:{instance}
func JobHeartbeatKey(instance string) string { return "job:heartbeat:" + instance }

// K26 bal:{userID}
func BalanceKey(userID int64) string {
	return "bal:" + strconv.FormatInt(userID, 10)
}

// K27 exp:user:{user_id}
//
// The console export lock (T14 A11). SETNX with a 1h TTL is the whole
// concurrency rule: a user may hold exactly one running export at a time, and
// the lock expires with the download token so a crashed worker cannot wedge
// the account forever. The value is fixed ("1"); the key's existence is the
// fact, never a task id, because the task id is an HMAC token the client
// already holds.
func ExportLockKey(userID int64) string {
	return "exp:user:" + strconv.FormatInt(userID, 10)
}

// K28 auth:fail:acct:{sha256(identifier)}
//
// Login failure counter for one account identifier. The identifier is an email
// or phone — PII that must not appear in MONITOR output or a keyspace dump — so
// the key carries its SHA-256 instead. The value is an INCR counter; the TTL is
// the lock window and is refreshed when the threshold is crossed.
func LoginAccountFailKey(identifier string) string {
	return "auth:fail:acct:" + sha256Hex(identifier)
}

// K29 auth:fail:ip:{sha256(ip)}
//
// Login failure counter for one client IP. Hashing keeps IPv6 colons out of the
// colon-separated namespace and keeps the full address out of Redis dumps. A
// missing/unparseable peer address is not counted: there is nothing to
// attribute it to, and hashing "" would merge every anonymous caller into one
// counter.
func LoginIPFailKey(ip string) string {
	return "auth:fail:ip:" + sha256Hex(ip)
}

// K30 sess:admin:uid:{user_id}
//
// Set of sid values owned by one account (members are K20 suffixes). Password
// change enumerates this set and deletes every K20 so all devices are signed
// out; without it the only way to revoke "all sessions" is a keyspace scan.
// The TTL matches K20's sliding TTL and is refreshed on every session load, so
// the index never expires while a session it points at is still alive.
func AdminUserSessionSetKey(userID int64) string {
	return "sess:admin:uid:" + strconv.FormatInt(userID, 10)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// IdemHash folds the idempotency identity required by §1.6 into the K6/K7
// hash segment. sha1 is used because the key template is defined with it and
// collisions here only affect cache placement, never authorization: the DB
// uniqueness key remains (scope, api_key_id, idem_key) and is checked on
// every miss.
func IdemHash(scope string, apiKeyID int64, idemKey string) string {
	sum := sha1.Sum([]byte(scope + "|" + strconv.FormatInt(apiKeyID, 10) + "|" + idemKey))
	return hex.EncodeToString(sum[:])
}

// Verification-code keys (K-VC series). The identifier is hashed by the caller
// before it becomes part of a key name: Redis is readable by anyone with access
// to the instance, and an e-mail address in a key would hand over the user
// list. Purpose is "login" or "register" so one mailbox can hold both codes
// without the login code being usable to register (and vice versa).
func VerifyCodeKey(purpose, identHash string) string {
	return "vc:code:" + purpose + ":" + identHash
}

func VerifyCodeCooldownKey(purpose, identHash string) string {
	return "vc:cool:" + purpose + ":" + identHash
}

func VerifyCodeQuotaKey(purpose, identHash string) string {
	return "vc:quota:" + purpose + ":" + identHash
}

// VerifyCodeIPQuotaKey 是按来源 IP 的小时配额。按邮箱的两条限流（冷却 + 每日）
// 挡不住「换一批收件人继续发」的脚本：每个新地址都是全新的冷却与配额，而消耗的
// 是我们的发信额度与发件域名信誉。window 是「年-月-日-时」，每小时一个键。
func VerifyCodeIPQuotaKey(ipHash, window string) string {
	return "vc:ipq:" + ipHash + ":" + window
}
