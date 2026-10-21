// Package sandbox 定义沙箱接口、执行规格、产物白名单和稳定错误码。
//
// 平台实现（darwin / linux / windows）和资源限制 shim 不在本包的本次范围内。
// Core 只依赖 Sandbox；测试和还没接上平台实现的调用方使用 Fake。
// 运行时不可用返回 runtime_unavailable，隔离机制不可用返回 sandbox_unavailable。
// 这两种情况都不得降级为不经沙箱直接执行。
package sandbox
