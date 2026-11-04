// Command seedctl creates the minimum local dataset needed to exercise the
// real request path: one user, one funded account, one API key and the caches
// that gatewayd reads.
//
// It refuses to run outside ENV=local. Production data is created through the
// control plane, never by a command that prints credentials.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"yunshu.dev/core/internal/apikey"
	"yunshu.dev/core/internal/boot"
	"yunshu.dev/core/internal/config"
	"yunshu.dev/core/internal/control/audit"
	"yunshu.dev/core/internal/control/auth"
	"yunshu.dev/core/internal/cryptobox"
	"yunshu.dev/core/internal/jsonx"
	"yunshu.dev/core/internal/ledger"
	"yunshu.dev/core/internal/platform/clock"
	"yunshu.dev/core/internal/platform/db"
	"yunshu.dev/core/internal/platform/phoneclaim"
	"yunshu.dev/core/internal/redisx"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seedctl:", err)
		os.Exit(1)
	}
}

func run() error {
	if env := strings.TrimSpace(os.Getenv("ENV")); !strings.EqualFold(env, "local") {
		return fmt.Errorf("seedctl is local-only (ENV=%q)", env)
	}

	email := flag.String("email", "local@yunshu.dev", "account email")
	phone := flag.String("phone", "", "account phone (E.164 without +); email wins when both are set")
	password := flag.String("password", os.Getenv("SEED_PASSWORD"), "account password (required; can use SEED_PASSWORD)")
	nickname := flag.String("nickname", "本地联调", "display name")
	modelName := flag.String("model", "local-qwen-plus", "model granted to the key; falls back to the first LISTED model")
	keyName := flag.String("key-name", "local-dev-key", "API key name")
	channelID := flag.Int64("channel-id", 0, "channel to repoint at fakeupstream; 0 finds/local-domestic-channel")
	channelURL := flag.String("channel-base-url", "", "channel base_url, e.g. http://127.0.0.1:18090/v1")
	channelKey := flag.String("channel-key", "", "upstream plaintext key; sealed with CHANNEL_KEY_KEK before the UPDATE")
	flag.Parse()

	if strings.TrimSpace(*password) == "" {
		return fmt.Errorf("-password or SEED_PASSWORD is required")
	}
	identRaw := *email
	if strings.TrimSpace(*email) == "" && strings.TrimSpace(*phone) != "" {
		identRaw = *phone
	}
	ident, err := auth.NormalizeIdentifier(identRaw)
	if err != nil {
		return fmt.Errorf("identifier: %w", err)
	}
	// seedctl builds a customer account with a funded balance and an API key,
	// and prints the plaintext key. A username account has no email and no
	// phone, so every later recovery path (verification code, operator lookup
	// by address) would be closed for it. Refusing is the honest answer; the
	// local database is reseeded with an email or a phone in seconds.
	if err := auth.RequireEmailOrPhone(ident); err != nil {
		return fmt.Errorf("seedctl 不接受用户名标识：%s（请用 -email 或 -phone）", identRaw)
	}

	dsn := strings.TrimSpace(os.Getenv("DB_DSN"))
	if dsn == "" {
		return fmt.Errorf("DB_DSN is not set")
	}
	conn, err := db.OpenMain(dsn, db.MainPoolMaxOpen, db.MainPoolMaxIdle)
	if err != nil {
		return fmt.Errorf("open mysql: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.Ping(ctx, conn); err != nil {
		return fmt.Errorf("ping mysql: %w", err)
	}

	rdb := redisx.New(redisx.Options{
		Addr:     strings.TrimSpace(os.Getenv("REDIS_ADDR")),
		Password: os.Getenv("REDIS_PASSWORD"),
		PoolSize: 10,
	})
	defer rdb.Close()
	if err := rdb.Ping(ctx); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}

	uid, err := createUser(ctx, conn, ident, *nickname, *password)
	if err != nil {
		return err
	}

	credits, err := fundAccount(ctx, conn, uid)
	if err != nil {
		return err
	}

	modelID, resolvedModel, err := pickModel(ctx, conn, *modelName)
	if err != nil {
		return err
	}
	kid, plaintext, err := createKey(ctx, conn, uid, *keyName, modelID)
	if err != nil {
		return err
	}

	if strings.TrimSpace(*channelURL) != "" || strings.TrimSpace(*channelKey) != "" {
		if err := repointChannel(ctx, conn, *channelID, *channelURL, *channelKey); err != nil {
			return err
		}
	}

	cfgRedis := config.NewGoRedis(rdb.Raw())
	pub := &config.Publisher{DB: conn, Redis: cfgRedis, Now: clock.Real{}.Now}
	routeBuilder := &boot.RouteSnapshot{DB: conn, Publisher: pub, Now: clock.Real{}}
	stats, result, err := routeBuilder.Publish(ctx)
	if err != nil {
		return err
	}
	caches := &boot.CacheWriter{
		DB:     conn,
		Prices: &config.PriceCache{Redis: cfgRedis},
		Keys:   redisx.NewKeyAuthCache(rdb),
		Now:    clock.Real{},
	}
	priceCount, err := caches.RefreshPrices(ctx)
	if err != nil {
		return err
	}
	keyCount, err := caches.RefreshKeys(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("seedctl: user_id=%d identifier=%s role=PERSONAL balance_micro=%d\n", uid, ident.Value, credits)
	fmt.Printf("seedctl: model_id=%d model_name=%s\n", modelID, resolvedModel)
	fmt.Printf("seedctl: api_key_id=%d prefix=%s last4=%s group=standard\n",
		kid, apikey.DisplayPrefix(plaintext), apikey.Last4(plaintext))
	fmt.Printf("seedctl: ROUTING version=%d models=%d routes=%d channels=%d groups=%d keys=%d key_models=%d bytes=%d took_ms=%d\n",
		result.Version, stats.Models, stats.Routes, stats.Channels, stats.Groups,
		stats.Keys, stats.KeyModels, stats.Bytes, stats.Took.Milliseconds())
	fmt.Printf("seedctl: K19 models=%d K13 keys=%d\n", priceCount, keyCount)
	// The plaintext key is printed exactly once and never stored or logged.
	fmt.Printf("seedctl: API_KEY=%s\n", plaintext)
	return nil
}

func createUser(ctx context.Context, conn *sql.DB, ident auth.Identifier, nickname, password string) (int64, error) {
	exists, err := userExists(ctx, conn, ident)
	if err != nil {
		return 0, err
	}
	if exists {
		return 0, fmt.Errorf("%s %q already exists; delete it or seed a different identifier", ident.Kind, ident.Value)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return 0, fmt.Errorf("hash password: %w", err)
	}
	now := clock.Real{}.Now()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var email, phone, phoneHMAC any
	if ident.Kind == auth.KindEmail {
		email = ident.Value
	} else {
		phone = ident.Value
		// The seeded account is a real row in a schema that carries
		// uk_user_phone_hmac: a phone seed without the hash would occupy no
		// uniqueness at all, so the next control-plane registration of the same
		// number would create a second account. Refusing here (exit 1) beats a
		// seed that behaves differently from production.
		//
		// The hmac input must be the canonical value, never the raw -phone
		// string: NormalizeIdentifier already produced it, and the assertion
		// below keeps that true if this function is ever handed a value from
		// somewhere else.
		canonical, ok := auth.CanonicalPhone(ident.Value)
		if !ok || canonical != ident.Value {
			return 0, fmt.Errorf("-phone %q 不是规范形态（期望 %q）", ident.Value, canonical)
		}
		key := strings.TrimSpace(os.Getenv(config.EnvPhoneHMACKey))
		mac, macErr := auth.PhoneHMAC(key, canonical)
		if macErr != nil {
			return 0, fmt.Errorf("手机号种子需要 %s（≥32 字节）：%w", config.EnvPhoneHMACKey, macErr)
		}
		phoneHMAC = mac
	}
	var uid int64
	for attempt := 0; attempt < 5; attempt++ {
		userNo, err := userNo(now)
		if err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO t_user
				(user_no, email, phone, phone_hmac, password_hash, password_algo, password_updated_at,
				 nickname, role, status, created_at, updated_at)
			VALUES (?,?,?,?,?,'bcrypt',?,?,'PERSONAL','NORMAL',?,?)`,
			userNo, email, phone, phoneHMAC, string(hash), now, nickname, now, now)
		if err == nil {
			uid, err = res.LastInsertId()
			if err != nil {
				return 0, err
			}
			break
		}
		if !strings.Contains(err.Error(), "uk_user_no") {
			return 0, fmt.Errorf("insert user: %w", err)
		}
	}
	if uid == 0 {
		return 0, fmt.Errorf("could not allocate a unique user_no")
	}
	// Same ledger row every other write path creates (auth.insertUserRow and
	// ops.CreateOperatorUser both do this through InsertUserRowTx). Skipping it
	// would not model "production has one row fewer": the trial claim on a
	// seeded phone account would answer 500 (fail closed, by design), parking
	// local development on a failure that cannot happen in production.
	if ident.Kind == auth.KindPhone {
		mac, _ := phoneHMAC.(string)
		if err := phoneclaim.UpsertOnRegisterTx(ctx, tx, mac, uid, now); err != nil {
			return 0, fmt.Errorf("upsert phone claim: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO t_credit_account (user_id, created_at, updated_at) VALUES (?,?,?)`,
		uid, now, now); err != nil {
		return 0, fmt.Errorf("insert credit account: %w", err)
	}
	masked := ident.Value
	if ident.Kind == auth.KindEmail {
		if at := strings.IndexByte(masked, '@'); at > 0 {
			masked = masked[:1] + "***" + masked[at:]
		}
	}
	after, err := jsonx.Marshal(map[string]string{
		"identifier":      masked,
		"identifier_kind": ident.Kind,
		"role":            "PERSONAL",
		"status":          "NORMAL",
	})
	if err != nil {
		return 0, err
	}
	writer := &audit.Writer{DB: conn, Now: clock.Real{}}
	if err := writer.Insert(ctx, tx, audit.Entry{
		OperatorID:   uid,
		OperatorRole: "PERSONAL",
		Module:       "USER",
		Action:       "CREATE",
		TargetType:   "t_user",
		TargetID:     strconv.FormatInt(uid, 10),
		After:        []byte(after),
		Result:       "OK",
	}); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}

func userExists(ctx context.Context, conn *sql.DB, ident auth.Identifier) (bool, error) {
	column := "email"
	if ident.Kind == auth.KindPhone {
		column = "phone"
	}
	var n int
	err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM t_user WHERE `+column+` = ?`, ident.Value).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check user: %w", err)
	}
	return n > 0, nil
}

// fundAccount writes 100,000 micro-credits through the ledger, not by updating
// the balance column: the batch and the append-only entry are what the daily
// reconciliation checks.
func fundAccount(ctx context.Context, conn *sql.DB, uid int64) (int64, error) {
	store := ledger.NewStore(conn, clock.Real{})
	result, err := store.Recharge(ctx, ledger.RechargeParams{
		UserID:      uid,
		AmountMicro: 100_000,
		IdemKey:     fmt.Sprintf("seed:recharge:%d", uid),
	})
	if err != nil {
		return 0, fmt.Errorf("seed recharge: %w", err)
	}
	return result.BalanceAfterMicro, nil
}

func pickModel(ctx context.Context, conn *sql.DB, wanted string) (int64, string, error) {
	var id int64
	var name string
	err := conn.QueryRowContext(ctx, `
		SELECT id, model_name FROM t_model
		 WHERE deleted_at IS NULL AND listing_state = 'LISTED' AND region = 'DOMESTIC'
		   AND model_name = ?`, wanted).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return 0, "", fmt.Errorf("lookup model %q: %w", wanted, err)
	}
	err = conn.QueryRowContext(ctx, `
		SELECT id, model_name FROM t_model
		 WHERE deleted_at IS NULL AND listing_state = 'LISTED' AND region = 'DOMESTIC'
		 ORDER BY id LIMIT 1`).Scan(&id, &name)
	if err != nil {
		return 0, "", fmt.Errorf("no LISTED DOMESTIC model: %w", err)
	}
	return id, name, nil
}

func createKey(ctx context.Context, conn *sql.DB, uid int64, name string, modelID int64) (int64, string, error) {
	plaintext, hash, err := apikey.Generate()
	if err != nil {
		return 0, "", err
	}
	now := clock.Real{}.Now()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	res, err := tx.ExecContext(ctx, `
		INSERT INTO t_api_key
			(user_id, key_name, key_prefix, key_hash, key_hash_version, key_last4,
			 status, model_scope_mode, ip_whitelist_mode, group_code, created_at, updated_at)
		VALUES (?,?,?,?,1,?,'ENABLED','SELECTED','OFF','standard',?,?)`,
		uid, name, apikey.DisplayPrefix(plaintext), hash, apikey.Last4(plaintext), now, now)
	if err != nil {
		return 0, "", fmt.Errorf("insert api key: %w", err)
	}
	kid, err := res.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO t_api_key_model (api_key_id, model_id, created_at) VALUES (?,?,?)`,
		kid, modelID, now); err != nil {
		return 0, "", fmt.Errorf("insert key model scope: %w", err)
	}
	after, err := jsonx.Marshal(map[string]any{
		"key_name":         name,
		"key_prefix":       apikey.DisplayPrefix(plaintext),
		"key_last4":        apikey.Last4(plaintext),
		"model_scope_mode": "SELECTED",
		"model_ids":        []int64{modelID},
		"group_code":       "standard",
	})
	if err != nil {
		return 0, "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO t_api_key_audit (api_key_id, action, before_json, after_json, operator_id, created_at)
		VALUES (?, 'CREATE', NULL, ?, ?, ?)`, kid, after, uid, now); err != nil {
		return 0, "", fmt.Errorf("insert api key audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", err
	}
	return kid, plaintext, nil
}

// repointChannel is the local-only escape hatch for the 0019 placeholder
// base_url. It never touches the migration files: the UPDATE runs against this
// database only, and the plaintext upstream key is sealed before it is stored.
func repointChannel(ctx context.Context, conn *sql.DB, channelID int64, baseURL, plaintextKey string) error {
	baseURL = strings.TrimSpace(baseURL)
	plaintextKey = strings.TrimSpace(plaintextKey)
	if baseURL == "" && plaintextKey == "" {
		return nil
	}
	if channelID <= 0 {
		err := conn.QueryRowContext(ctx, `
			SELECT id FROM t_channel
			 WHERE deleted_at IS NULL
			 ORDER BY (name = 'local-domestic-channel') DESC, id LIMIT 1`).Scan(&channelID)
		if err != nil {
			return fmt.Errorf("find channel: %w", err)
		}
	}
	var currentURL string
	var currentEnc []byte
	var currentMask string
	err := conn.QueryRowContext(ctx, `
		SELECT base_url, upstream_key_enc, upstream_key_mask
		  FROM t_channel WHERE id = ? AND deleted_at IS NULL`, channelID,
	).Scan(&currentURL, &currentEnc, &currentMask)
	if err == sql.ErrNoRows {
		return fmt.Errorf("channel %d not found", channelID)
	}
	if err != nil {
		return fmt.Errorf("read channel %d: %w", channelID, err)
	}
	if baseURL == "" {
		baseURL = currentURL
	}
	enc, mask := currentEnc, currentMask
	if plaintextKey != "" {
		box, err := cryptobox.FromEnv()
		if err != nil {
			return err
		}
		sealed, err := box.Seal([]byte(plaintextKey))
		if err != nil {
			return err
		}
		enc = []byte(sealed)
		mask = maskKey(plaintextKey)
	}
	if _, err := conn.ExecContext(ctx, `
		UPDATE t_channel
		   SET base_url = ?, upstream_key_enc = ?, upstream_key_mask = ?, updated_at = ?
		 WHERE id = ? AND deleted_at IS NULL`,
		baseURL, enc, mask, clock.Real{}.Now(), channelID); err != nil {
		return fmt.Errorf("update channel %d: %w", channelID, err)
	}
	fmt.Printf("seedctl: channel_id=%d base_url=%s key_mask=%s\n", channelID, baseURL, mask)
	return nil
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:3] + "****" + key[len(key)-4:]
}

func userNo(now time.Time) (string, error) {
	// 12 characters: U + yyyymmdd + 3 digits. See the T11 receipt for the
	// char(12) versus "4位序号" discrepancy.
	n := (int(now.UnixNano()>>8) ^ os.Getpid()) % 1000
	return fmt.Sprintf("U%s%03d", now.Format("20060102"), n), nil
}
