// Package seed 生成星禾、云杉两个示例工作区。
// 订单、订单明细、商品、用户、退款写入 analytics.db。
// 手机号和邮箱只由 writeSensitive 写入 sensitive.db。
package seed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/loveRyujin/lantern/core/workspace"
)

// errForceBusy 表示同一数据目录已经有一次 -force 拿着文件锁。
var errForceBusy = errors.New("seed: 已有一次 -force 在进行")

// forceLockName 是数据目录下的 -force 文件锁。
// CleanScratch 只删 `.lantern-seed-` 前缀，不会把这个锁文件清掉。
const forceLockName = ".lantern-seed.lock"

// DefaultSeed 是命令行和 make seed 使用的随机种子。
const DefaultSeed uint64 = 20261021

// Options 控制一次生成。Seed 为 0 也是一个确定的种子，不会替换成 DefaultSeed。
type Options struct {
	Root  string
	Seed  uint64
	Force bool
}

// Summary 是一个工作区写完之后的规模和内容哈希。
type Summary struct {
	ID         string
	Dir        string
	Hash       string
	Users      int
	Products   int
	Orders     int
	OrderItems int
	Refunds    int
}

// Result 是两个工作区的生成结果。Hash 由两个工作区的内容哈希按固定顺序合成。
type Result struct {
	Root       string
	Seed       uint64
	Hash       string
	Workspaces []Summary
}

// Run 在 root 下创建星禾、云杉两个工作区并写入合成数据。
// root 为空时使用 workspace.DefaultRoot。
// Force 为真时先在临时目录生成两份，都成功后才替换原来的工作区。
// 生成失败时原来的目录保持不动。符号链接不会被跟着删掉。
func Run(ctx context.Context, opt Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, err := resolveRoot(opt.Root)
	if err != nil {
		return Result{}, err
	}
	if opt.Force {
		return runStaged(ctx, root, opt.Seed)
	}
	return runInPlace(ctx, root, opt.Seed)
}

func runInPlace(ctx context.Context, root string, seedN uint64) (Result, error) {
	return runProfiles(ctx, root, seedN, root)
}

func runStaged(ctx context.Context, root string, seedN uint64) (Result, error) {
	unlock, err := lockForce(root)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	if err := workspace.CleanScratch(root); err != nil {
		return Result{}, err
	}
	for _, profile := range profiles {
		if err := workspace.Replaceable(root, profile.ID); err != nil {
			return Result{}, err
		}
	}
	staging, err := workspace.ScratchDir(root)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(staging)

	out, err := runProfiles(ctx, root, seedN, staging)
	if err != nil {
		return Result{}, err
	}
	ids := make([]string, len(profiles))
	for i, profile := range profiles {
		ids[i] = profile.ID
	}
	dirs, err := workspace.Install(root, staging, ids)
	if err != nil {
		return Result{}, err
	}
	if len(dirs) != len(out.Workspaces) {
		return Result{}, fmt.Errorf("seed: 装回了 %d 个工作区，想要 %d", len(dirs), len(out.Workspaces))
	}
	for i := range out.Workspaces {
		out.Workspaces[i].Dir = dirs[i]
	}
	return out, nil
}

// lockForce 独占 root 下的 `.lantern-seed.lock`。拿不到就返回 errForceBusy，不清理临时目录。
// 进程退出时锁会放开，包括中途崩溃。
func lockForce(root string) (func(), error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("seed: 数据目录: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(root, forceLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("seed: 文件锁: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errForceBusy
		}
		return nil, fmt.Errorf("seed: 文件锁: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func runProfiles(ctx context.Context, resultRoot string, seedN uint64, writeRoot string) (Result, error) {
	out := Result{Root: resultRoot, Seed: seedN, Workspaces: make([]Summary, 0, len(profiles))}
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		sum, err := seedProfile(ctx, writeRoot, seedN, profile)
		if err != nil {
			return Result{}, err
		}
		out.Workspaces = append(out.Workspaces, sum)
	}
	combined, err := combineHash(out.Workspaces)
	if err != nil {
		return Result{}, err
	}
	out.Hash = combined
	return out, nil
}

func resolveRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		var err error
		root, err = workspace.DefaultRoot()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("seed: 数据目录: %w", err)
	}
	return filepath.Clean(abs), nil
}

// seedProfile 生成一个工作区。测试替换它，用来模拟后面的工作区失败。
var seedProfile = seedWorkspace

func seedWorkspace(ctx context.Context, root string, seedN uint64, profile profile) (sum Summary, err error) {
	data, err := generate(profile, seedN)
	if err != nil {
		return Summary{}, err
	}
	h, err := workspace.Create(ctx, root, profile.ID)
	if err != nil {
		if errors.Is(err, workspace.ErrExists) {
			return Summary{}, fmt.Errorf("seed: 工作区 %s 已存在，使用 -force 删除后重建: %w", profile.ID, err)
		}
		return Summary{}, err
	}
	ok := false
	defer func() {
		cerr := h.Close()
		if !ok {
			_ = workspace.Remove(root, profile.ID)
			return
		}
		if err == nil && cerr != nil {
			err = fmt.Errorf("seed: 关闭工作区 %s: %w", profile.ID, cerr)
		}
	}()

	h.Analytics.SetMaxOpenConns(1)
	if err = writeAnalytics(ctx, h.Analytics, data); err != nil {
		return Summary{}, err
	}
	sensitive, err := writeSensitive(ctx, h, data.contacts())
	if err != nil {
		return Summary{}, err
	}
	if err = checkpoint(ctx, h.Analytics); err != nil {
		return Summary{}, err
	}
	if err = verifyCounts(ctx, h.Analytics, sensitive, data); err != nil {
		return Summary{}, err
	}
	digest, err := hashWorkspace(ctx, profile.ID, h.Analytics, sensitive)
	if err != nil {
		return Summary{}, err
	}
	sum = Summary{
		ID:         profile.ID,
		Dir:        h.Dir,
		Hash:       digest,
		Users:      len(data.users),
		Products:   len(data.products),
		Orders:     len(data.orders),
		OrderItems: len(data.items),
		Refunds:    len(data.refunds),
	}
	ok = true
	return sum, nil
}

func writeAnalytics(ctx context.Context, db *sql.DB, data dataset) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("seed: 分析库事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := insertUsers(ctx, tx, data.users); err != nil {
		return err
	}
	if err := insertProducts(ctx, tx, data.products); err != nil {
		return err
	}
	if err := insertOrders(ctx, tx, data.orders); err != nil {
		return err
	}
	if err := insertItems(ctx, tx, data.items); err != nil {
		return err
	}
	if err := insertRefunds(ctx, tx, data.refunds); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("seed: 提交分析库: %w", err)
	}
	return nil
}

func insertUsers(ctx context.Context, tx *sql.Tx, rows []userRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO users (id, display_name, city, member_level) VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备用户写入: %w", err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if err := screenAnalyticsText("users.display_name", row.Name); err != nil {
			return err
		}
		if err := screenAnalyticsText("users.city", row.City); err != nil {
			return err
		}
		if err := screenAnalyticsText("users.member_level", row.Level); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, row.ID, row.Name, row.City, row.Level); err != nil {
			return fmt.Errorf("seed: 写入用户 %s: %w", row.ID, err)
		}
	}
	return nil
}

func insertProducts(ctx context.Context, tx *sql.Tx, rows []productRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO products (id, name, category, price_cents) VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备商品写入: %w", err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if err := screenAnalyticsText("products.name", row.Name); err != nil {
			return err
		}
		if err := screenAnalyticsText("products.category", row.Category); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, row.ID, row.Name, row.Category, row.PriceCents); err != nil {
			return fmt.Errorf("seed: 写入商品 %s: %w", row.ID, err)
		}
	}
	return nil
}

func insertOrders(ctx context.Context, tx *sql.Tx, rows []orderRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO orders (id, user_id, created_at, status, amount_cents) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备订单写入: %w", err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if err := screenAnalyticsText("orders.created_at", row.CreatedAt); err != nil {
			return err
		}
		if err := screenAnalyticsText("orders.status", row.Status); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, row.ID, row.UserID, row.CreatedAt, row.Status, row.AmountCents); err != nil {
			return fmt.Errorf("seed: 写入订单 %s: %w", row.ID, err)
		}
	}
	return nil
}

func insertItems(ctx context.Context, tx *sql.Tx, rows []itemRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO order_items (id, order_id, product_id, qty, unit_price_cents) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备订单明细写入: %w", err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if _, err := stmt.ExecContext(ctx, row.ID, row.OrderID, row.ProductID, row.Qty, row.UnitPriceCents); err != nil {
			return fmt.Errorf("seed: 写入订单明细 %s: %w", row.ID, err)
		}
	}
	return nil
}

func insertRefunds(ctx context.Context, tx *sql.Tx, rows []refundRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO refunds (id, order_id, created_at, amount_cents, reason) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("seed: 准备退款写入: %w", err)
	}
	defer stmt.Close()
	for _, row := range rows {
		if err := screenAnalyticsText("refunds.created_at", row.CreatedAt); err != nil {
			return err
		}
		if err := screenAnalyticsText("refunds.reason", row.Reason); err != nil {
			return err
		}
		if _, err := stmt.ExecContext(ctx, row.ID, row.OrderID, row.CreatedAt, row.AmountCents, row.Reason); err != nil {
			return fmt.Errorf("seed: 写入退款 %s: %w", row.ID, err)
		}
	}
	return nil
}

var analyticsContact = regexp.MustCompile(`1[3-9]\d{9}`)

func screenAnalyticsText(field, value string) error {
	if strings.Contains(value, "@") || analyticsContact.MatchString(value) {
		return fmt.Errorf("seed: 拒绝把联系方式写入分析库字段 %s", field)
	}
	return nil
}

func verifyCounts(ctx context.Context, analytics, sensitive *sql.DB, data dataset) error {
	checks := []struct {
		db    *sql.DB
		table string
		want  int
	}{
		{analytics, "users", len(data.users)},
		{analytics, "products", len(data.products)},
		{analytics, "orders", len(data.orders)},
		{analytics, "order_items", len(data.items)},
		{analytics, "refunds", len(data.refunds)},
		{sensitive, "user_contacts", len(data.users)},
	}
	for _, check := range checks {
		var n int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s", check.table)
		if err := check.db.QueryRowContext(ctx, query).Scan(&n); err != nil {
			return fmt.Errorf("seed: 统计 %s: %w", check.table, err)
		}
		if n != check.want {
			return fmt.Errorf("seed: %s 行数 = %d，想要 %d", check.table, n, check.want)
		}
	}
	return nil
}

func checkpoint(ctx context.Context, db *sql.DB) error {
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("seed: checkpoint: %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("seed: checkpoint 未完成 (busy=%d)", busy)
	}
	return nil
}
