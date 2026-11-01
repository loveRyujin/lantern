-- 检索命中要带回标题路径和切分策略。旧库在打开工作区时补上这两列。

ALTER TABLE chunks ADD COLUMN title_path TEXT NOT NULL DEFAULT '';
ALTER TABLE chunks ADD COLUMN strategy TEXT NOT NULL DEFAULT '';
