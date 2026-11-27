package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// busyBudget 是一次写事务因 SQLITE_BUSY 重试的总时长。连接上的 busy_timeout 只管等锁；
// WAL 下事务先读后写、读快照又已过期时，SQLite 直接返回 SQLITE_BUSY，不走 busy handler，
// 只能整段重来。
var busyBudget = 5 * time.Second

// keep 让 fn 的写入随事务提交，同时把 err 交给调用方。
type keep struct{ err error }

func (k keep) Error() string { return k.err.Error() }

func (k keep) Unwrap() error { return k.err }

// writeTx 在一个事务里跑 fn。遇到 SQLITE_BUSY 就回滚并整段重跑，fn 必须每次都重新读状态。
// 预算用完仍然 BUSY 时返回包了 ErrBusy 的错误，什么都没有提交。
func (s *Service) writeTx(ctx context.Context, fn func(*sql.Tx) error) error {
	deadline := time.Now().Add(busyBudget)
	wait := 2 * time.Millisecond
	for {
		err := s.tryTx(ctx, fn)
		if !isBusy(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %v", ErrBusy, err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}

func (s *Service) tryTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	err = fn(tx)
	var k keep
	if errors.As(err, &k) {
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		return k.err
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// isBusy 认 SQLITE_BUSY 和 SQLITE_LOCKED（含扩展码）。驱动错误带 Code()。
func isBusy(err error) bool {
	if err == nil {
		return false
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5, 6:
			return true
		}
	}
	return false
}
