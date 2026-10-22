package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rollbackTimeout bounds the ROLLBACK that ends every transaction we begin.
const rollbackTimeout = 5 * time.Second

// withReadOnlyTx runs fn inside BEGIN READ ONLY and always ends the
// transaction with ROLLBACK, whatever fn returned, so nothing fn ran can
// persist and the pooled connection is always released. fn must finish with
// every pgx.Rows it opened before it returns.
func withReadOnlyTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	return fn(tx)
}

// rollback ends tx with ROLLBACK on a fresh short context, so a cancelled ctx
// cannot leave the transaction open on its connection. The error is dropped:
// a failed ROLLBACK means the connection is already unusable, and pgxpool
// discards it on release.
func rollback(ctx context.Context, tx pgx.Tx) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	_ = tx.Rollback(rctx)
}
