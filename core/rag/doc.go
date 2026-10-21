// Package rag 定义 Embedder、VectorStore，以及供测试使用的确定性 fake。
//
// 向量库实例绑定一个工作区。检索在内存里对已归一化的向量做点积，点积即余弦。
// 索引里的 embedder_id、维度和当前 Embedder 不一致时，写入和检索都返回
// embedding_mismatch，不返回命中，也不混入另一套向量。
// 默认的 SQLite 存储、Ollama 和 OpenAI Embedder 不在本包的本次范围内。
package rag
