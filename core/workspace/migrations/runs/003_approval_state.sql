-- 审批状态机的状态和乐观并发版本。
-- decision、decided_at、expires_at、consumed_at 继续当审计字段，不单独充当状态。
-- 存量行按这些审计字段回填。version 保持 0，下一次条件更新再加一。
-- 这里不按 expires_at 把行改成 expired，访问时再判断。

ALTER TABLE approvals ADD COLUMN state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE approvals ADD COLUMN version INTEGER NOT NULL DEFAULT 0;

UPDATE approvals
SET state = CASE
    WHEN consumed_at IS NOT NULL AND consumed_at <> '' THEN 'consumed'
    WHEN decision = 'approve' THEN 'approved'
    WHEN decision = 'reject' THEN 'rejected'
    ELSE 'pending'
END;
