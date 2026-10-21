-- 导出确认要核对：记录存在、属于本工作区、动作是 export_data、
-- 导出对象一致、决定为批准、未过期、且只能用一次。

ALTER TABLE approvals ADD COLUMN workspace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN action TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN export_object TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN consumed_at TEXT;
