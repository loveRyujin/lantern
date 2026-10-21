// Package llm 提供语言模型 Provider。
//
// Core 只依赖 [Provider]，本包不 import mygo。桌面端把 mygo.Secrets 适配成
// [SecretStore] 交给 [Grok]；headless 评测从环境变量 XAI_API_KEY（其次 GROK_API_KEY）读取密钥。
//
// [Grok] 是 grok-4.6 的 OpenAI 兼容 Chat Completions 薄客户端，支持 tool_calls 与 SSE 流式输出。
// 技术设计 v0.3 指定走这套接口，而不是 Responses API。
//
// [Mock] 按 [Script] 返回确定结果，并可注入超时、HTTP 5xx、非法 JSON、Ollama 不可达和流被截断。
// 故障可以写在若干流式增量之后，用来模拟流到一半失败。Ollama 不可达只是脚本故障，本包不会去连接 localhost:11434。
//
// 调用 [Provider.Chat] 时必须给 ctx 设置截止时间。默认 HTTP 客户端不设超时。
//
// 连真实 grok 的冒烟测试带有构建标签 grok。CI 执行的 go test ./... 不带该标签，不会编译也不会运行它们。
// 本地执行：
//
//	go test -tags=grok ./core/llm/
package llm
