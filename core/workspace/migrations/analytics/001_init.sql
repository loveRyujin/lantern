-- analytics.db：业务表。users 只有脱敏列，手机号和邮箱原文在 sensitive.db。

CREATE TABLE users (
    id TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    city TEXT NOT NULL DEFAULT '',
    member_level TEXT NOT NULL DEFAULT ''
);

CREATE TABLE products (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    category TEXT NOT NULL DEFAULT '',
    price_cents INTEGER NOT NULL
);

CREATE TABLE orders (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users (id),
    created_at TEXT NOT NULL,
    status TEXT NOT NULL,
    amount_cents INTEGER NOT NULL
);

CREATE TABLE order_items (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders (id),
    product_id TEXT NOT NULL REFERENCES products (id),
    qty INTEGER NOT NULL,
    unit_price_cents INTEGER NOT NULL
);

CREATE TABLE refunds (
    id TEXT PRIMARY KEY,
    order_id TEXT NOT NULL REFERENCES orders (id),
    created_at TEXT NOT NULL,
    amount_cents INTEGER NOT NULL,
    reason TEXT NOT NULL DEFAULT ''
);
