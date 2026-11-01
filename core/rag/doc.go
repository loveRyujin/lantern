// Package rag 定义 Embedder、VectorStore、文档切分和导入，以及它们的默认实现。
//
// 向量库实例绑定一个工作区自己的 kb.db。检索在内存里对已归一化的向量做点积，点积即余弦。
// 索引里的 embedder_id、维度和当前 Embedder 不一致时，写入和检索都返回
// embedding_mismatch，不返回命中，也不混入另一套向量。
// 默认存储是 SQLiteStore（float32 BLOB，暴力点积）。默认 Embedder 是本机 Ollama 的 bge-m3。
// 导入只接受 Markdown 和 TXT。fixed 按 Unicode 码点开窗，structural 按标题路径和口径条目切开。
// 嵌入失败时不会改索引；SQLite 上同一文档的替换在一个事务里完成。
// FakeStore 和 FakeEmbedder 不访问磁盘或网络。OpenAI Embedder、PDF、search_docs 仍不在本包范围内。
package rag
