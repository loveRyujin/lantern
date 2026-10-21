package writebypass

// 拼接和多行原始字符串都要被扫出来。
const inserted = "INSERT INTO " + "approvals" + " (id) VALUES ('x')"

const updated = `
UPDATE approvals
SET decision = 'approve'
WHERE id = 'x'
`
