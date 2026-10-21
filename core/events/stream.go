package events

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed 表示事件流已经关闭，新的 Publish 不会再进入通道。
var ErrClosed = errors.New("events: 事件流已关闭")

// Publisher 是 Core 写入事件的出口。
// Stream 给 UI 和评测 CLI 使用，Recorder 给测试使用。两者都可以替换，Core 不依赖 mygo。
type Publisher interface {
	Publish(ctx context.Context, ev Event) error
}

// Stream 把事件送到带缓冲的 channel。
//
// 只有内部的泵 goroutine 会向该 channel 发送并在结束时关闭它，因此 Publish 和 Close 可以并发。
// Publish 返回 nil 时，事件已经进入缓冲或已经被取走。
// 缓冲满时 Publish 阻塞，直到有空间、ctx 结束，或 Close。
//
// 调用方必须 Close，否则泵不会退出。Close 可以重复调用。
// Close 返回后，Events 里已经写入的事件仍可读取，随后通道关闭。
type Stream struct {
	in     chan delivery
	out    chan Event
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type delivery struct {
	ev  Event
	ack chan error
	ctx context.Context
}

var _ Publisher = (*Stream)(nil)

// NewStream 启动事件流。buffer 小于 0 时按 0 处理，Publish 会等到消费方取走事件才返回。
func NewStream(buffer int) *Stream {
	if buffer < 0 {
		buffer = 0
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stream{
		in:     make(chan delivery),
		out:    make(chan Event, buffer),
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.pump()
	return s
}

// Publish 校验并发送一条事件。Seq 从 1 开始按送达顺序递增，失败的发送不占序号。
func (s *Stream) Publish(ctx context.Context, ev Event) error {
	if ctx == nil {
		return errors.New("events: ctx 不能为 nil")
	}
	ev, err := prepare(ev, time.Now())
	if err != nil {
		return err
	}
	ack := make(chan error, 1)
	d := delivery{ev: ev, ack: ack, ctx: ctx}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return ErrClosed
	case s.in <- d:
	}
	return <-ack
}

// Events 返回只读通道。Close 完成之后该通道会被关闭。
func (s *Stream) Events() <-chan Event { return s.out }

// Close 拒绝后续 Publish，并把已经进入输出缓冲的事件留在通道里。
func (s *Stream) Close() {
	s.once.Do(func() {
		s.cancel()
		<-s.done
	})
}

func (s *Stream) pump() {
	defer close(s.done)
	defer close(s.out)
	var n uint64
	for {
		select {
		case <-s.ctx.Done():
			return
		case d := <-s.in:
			err := s.deliver(&n, d)
			d.ack <- err
			if errors.Is(err, ErrClosed) {
				return
			}
		}
	}
}

func (s *Stream) deliver(n *uint64, d delivery) error {
	if err := d.ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return ErrClosed
	}
	*n++
	ev := d.ev
	ev.Seq = *n
	select {
	case <-d.ctx.Done():
		*n--
		return d.ctx.Err()
	case <-s.ctx.Done():
		*n--
		return ErrClosed
	case s.out <- ev:
		return nil
	}
}
