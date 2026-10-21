package agent

import "testing"

func TestFailureCounterExecutionDoesNotChangeCount(t *testing.T) {
	var c failureCounter
	if c.noteArgumentStep() || c.noteArgumentStep() {
		t.Fatal("两次参数失败还不该结束")
	}
	if c.n != 2 {
		t.Fatalf("计数 = %d，想要 2", c.n)
	}
	c.noteExecutionFailure()
	if c.n != 2 {
		t.Fatalf("执行失败后计数 = %d，不应计入也不应清零", c.n)
	}
	c.noteSuccess()
	if c.n != 0 {
		t.Fatalf("成功后计数 = %d，想要 0", c.n)
	}
	c.noteExecutionFailure()
	if c.n != 0 {
		t.Fatal("成功清零之后，执行失败不应把计数加回来")
	}
}

func TestFailureCounterStopsAtLimit(t *testing.T) {
	var c failureCounter
	if c.noteArgumentStep() || c.noteArgumentStep() {
		t.Fatal("未到上限")
	}
	if !c.noteArgumentStep() {
		t.Fatal("第三次参数失败应该结束运行")
	}
}
