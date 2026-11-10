-- 确认单冻结 Policy 当时给出的风险说明。
-- 旧行没有这份说明，保持空字符串。空字符串表示未冻结，
-- 界面必须明示缺失，不能用模型回复或事后重算补上。

ALTER TABLE approvals ADD COLUMN risk TEXT NOT NULL DEFAULT '';
