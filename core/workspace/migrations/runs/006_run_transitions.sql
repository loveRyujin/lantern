-- 运行状态迁移，只允许追加。轨迹视图按时间读它。
-- run_events 同样只追加，这里一并补上触发器。
-- runs 的写入方不止一处（Agent 循环、重启恢复、审批状态机、Resume），
-- 由触发器在同一事务里记下，写入方不用各自补一行。
-- 本迁移之前的运行没有迁移记录，轨迹只能显示当前状态。

CREATE TABLE run_transitions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    from_state TEXT NOT NULL,
    to_state TEXT NOT NULL,
    at TEXT NOT NULL
);

CREATE INDEX run_transitions_run_id ON run_transitions (run_id);

CREATE TRIGGER runs_record_created
AFTER INSERT ON runs
BEGIN
    INSERT INTO run_transitions (run_id, from_state, to_state, at)
    VALUES (NEW.id, '', NEW.state, NEW.created_at);
END;

CREATE TRIGGER runs_record_transition
AFTER UPDATE OF state ON runs
WHEN NEW.state IS NOT OLD.state
BEGIN
    INSERT INTO run_transitions (run_id, from_state, to_state, at)
    VALUES (NEW.id, OLD.state, NEW.state, NEW.updated_at);
END;

CREATE TRIGGER run_transitions_forbid_update
BEFORE UPDATE ON run_transitions
BEGIN
    SELECT RAISE(ABORT, 'run_transitions is append-only');
END;

CREATE TRIGGER run_transitions_forbid_delete
BEFORE DELETE ON run_transitions
BEGIN
    SELECT RAISE(ABORT, 'run_transitions is append-only');
END;

CREATE TRIGGER run_events_forbid_update
BEFORE UPDATE ON run_events
BEGIN
    SELECT RAISE(ABORT, 'run_events is append-only');
END;

CREATE TRIGGER run_events_forbid_delete
BEFORE DELETE ON run_events
BEGIN
    SELECT RAISE(ABORT, 'run_events is append-only');
END;
