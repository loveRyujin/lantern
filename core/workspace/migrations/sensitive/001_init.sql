-- sensitive.db：手机号和邮箱原文。只有确认导出之后，业务代码才应读取这个库。
-- user_id 与 analytics.users.id 对应，两个库没有跨库外键。

CREATE TABLE user_contacts (
    user_id TEXT PRIMARY KEY,
    phone TEXT NOT NULL DEFAULT '',
    email TEXT NOT NULL DEFAULT ''
);
