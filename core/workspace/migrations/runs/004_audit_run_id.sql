-- 审计按 run_id 查询。旧行保持空字符串。
-- 新的恢复事件和 audit.SQLite 写入这一列。本列不放参数原文。

ALTER TABLE audit ADD COLUMN run_id TEXT NOT NULL DEFAULT '';

CREATE INDEX audit_run_id ON audit (run_id);
