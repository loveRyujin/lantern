-- kb.db：文档、切片、向量，以及当前索引使用的嵌入模型。
-- index_meta 只允许 id = 1 这一行；没有记录表示还没有建过索引。

CREATE TABLE documents (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE index_meta (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    embedder_id TEXT NOT NULL,
    dim INTEGER NOT NULL,
    normalize INTEGER NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE chunks (
    id TEXT PRIMARY KEY,
    document_id TEXT NOT NULL REFERENCES documents (id),
    ordinal INTEGER NOT NULL,
    text TEXT NOT NULL,
    embedder_id TEXT NOT NULL DEFAULT '',
    UNIQUE (document_id, ordinal)
);

CREATE TABLE vectors (
    chunk_id TEXT PRIMARY KEY REFERENCES chunks (id),
    embedder_id TEXT NOT NULL,
    dim INTEGER NOT NULL,
    embedding BLOB NOT NULL
);
