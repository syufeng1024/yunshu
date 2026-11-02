module yunshu.dev/core

go 1.27

require (
	github.com/go-sql-driver/mysql v1.10.1
	github.com/redis/go-redis/v9 v9.22.0
	golang.org/x/crypto v0.33.0
	yunshu.dev/relaykit v0.0.0
)

replace yunshu.dev/relaykit => ./relaykit

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.uber.org/atomic v1.12.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
)
