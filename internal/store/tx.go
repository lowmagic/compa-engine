package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Querier runs statements: on the store's database, or within a
// transaction on it.
type Querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is a transaction: its statements apply together on Commit, or not at
// all. Rollback after Commit does nothing, so it can be deferred.
type Tx interface {
	Querier
	Commit() error
	Rollback() error
}

// DB is a store's database as its packages use it. Within Store.Tx, Begin
// opens a savepoint in the transaction, so a package's own transaction
// becomes part of the larger one.
type DB interface {
	Querier
	Begin() (Tx, error)
}

type rootDB struct{ *sql.DB }

func (r rootDB) Begin() (Tx, error) { return r.BeginTx(context.Background(), nil) }

// txDB is a transaction as a DB: its statements run in the transaction,
// and Begin nests a savepoint.
type txDB struct {
	*sql.Tx
	n *int
}

func (t txDB) Begin() (Tx, error) {
	*t.n++
	sp := &savepoint{Tx: t.Tx, name: fmt.Sprintf("sp%d", *t.n)}
	if _, err := t.ExecContext(context.Background(), "SAVEPOINT "+sp.name); err != nil {
		return nil, err
	}
	return sp, nil
}

type savepoint struct {
	*sql.Tx
	name string
	done bool
}

func (s *savepoint) Commit() error {
	if s.done {
		return sql.ErrTxDone
	}
	s.done = true
	_, err := s.ExecContext(context.Background(), "RELEASE "+s.name)
	return err
}

func (s *savepoint) Rollback() error {
	if s.done {
		return sql.ErrTxDone
	}
	s.done = true
	if _, err := s.ExecContext(context.Background(), "ROLLBACK TO "+s.name); err != nil {
		return err
	}
	_, err := s.ExecContext(context.Background(), "RELEASE "+s.name)
	return err
}

// Tx runs fn with a store bound to one transaction begun under ctx:
// everything written through it, by any package, is stored together when
// fn returns nil, or not at all. The store has one connection, which the
// transaction holds until fn returns, so fn must reach the database only
// through the store it is given; a statement on s itself would wait for
// ever.
func (s *Store) Tx(ctx context.Context, fn func(*Store) error) error {
	if s.bound {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	b := &Store{dir: s.dir, now: s.now, maxBlob: s.maxBlob, enc: s.enc, dec: s.dec, q: txDB{Tx: tx, n: new(int)}, bound: true}
	if err := fn(b); err != nil {
		return errors.Join(err, ignoreDone(tx.Rollback()))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func ignoreDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}
