package agent

// DefaultFailureLimit 是整次运行连续参数校验失败的上限。
// 计满这个数，运行状态变为 failed，错误 Retryable 为 true。
// 只统计截断、非法 JSON 和 Schema 不符。一步里多个调用不合法只计 1。
// 工具执行失败不计入，也不清零。只有真正执行成功才清零。
const DefaultFailureLimit = 3

// failureCounter 把上面的规则收在一处。要改口径，只改这个类型。
type failureCounter struct {
	n int
}

// noteArgumentStep 记一次参数校验失败（一步一次）。达到上限时返回 true。
func (c *failureCounter) noteArgumentStep() bool {
	c.n++
	return c.n >= DefaultFailureLimit
}

// noteSuccess 在一次工具调用真正成功后把计数清零。
func (c *failureCounter) noteSuccess() {
	c.n = 0
}

// noteExecutionFailure 参数合法但执行失败。不计入，也不清零。
func (c *failureCounter) noteExecutionFailure() {}
