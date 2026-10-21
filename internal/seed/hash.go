package seed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type hashTable struct {
	name  string
	db    *sql.DB
	query string
}

func hashWorkspace(ctx context.Context, id string, analytics, sensitive *sql.DB) (string, error) {
	hw := sha256.New()
	if _, err := fmt.Fprintf(hw, "workspace %s\n", id); err != nil {
		return "", err
	}
	tables := []hashTable{
		{name: "users", db: analytics, query: `SELECT id, display_name, city, member_level FROM users ORDER BY id`},
		{name: "products", db: analytics, query: `SELECT id, name, category, price_cents FROM products ORDER BY id`},
		{name: "orders", db: analytics, query: `SELECT id, user_id, created_at, status, amount_cents FROM orders ORDER BY id`},
		{name: "order_items", db: analytics, query: `SELECT id, order_id, product_id, qty, unit_price_cents FROM order_items ORDER BY id`},
		{name: "refunds", db: analytics, query: `SELECT id, order_id, created_at, amount_cents, reason FROM refunds ORDER BY id`},
		{name: "user_contacts", db: sensitive, query: `SELECT user_id, phone, email FROM user_contacts ORDER BY user_id`},
	}
	for _, table := range tables {
		if err := hashQuery(ctx, hw, table); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hw.Sum(nil)), nil
}

func combineHash(parts []Summary) (string, error) {
	hw := sha256.New()
	for _, part := range parts {
		if _, err := fmt.Fprintf(hw, "%s %s\n", part.ID, part.Hash); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hw.Sum(nil)), nil
}

func hashQuery(ctx context.Context, hw io.Writer, table hashTable) error {
	rows, err := table.db.QueryContext(ctx, table.query)
	if err != nil {
		return fmt.Errorf("seed: 读取 %s: %w", table.name, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("seed: %s 列: %w", table.name, err)
	}
	if _, err := fmt.Fprintf(hw, "table %s\n%s\n", table.name, strings.Join(cols, "|")); err != nil {
		return err
	}

	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		for i := range vals {
			vals[i] = nil
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("seed: 扫描 %s: %w", table.name, err)
		}
		for i, v := range vals {
			if i > 0 {
				if _, err := hw.Write([]byte{0x1f}); err != nil {
					return err
				}
			}
			text, err := formatValue(v)
			if err != nil {
				return fmt.Errorf("seed: %s: %w", table.name, err)
			}
			if _, err := io.WriteString(hw, text); err != nil {
				return err
			}
		}
		if _, err := hw.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("seed: 读取 %s: %w", table.name, err)
	}
	return nil
}

func formatValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case []byte:
		return string(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case int:
		return strconv.Itoa(t), nil
	default:
		return "", fmt.Errorf("不支持的列类型 %T", v)
	}
}
