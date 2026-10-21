package llm

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

const maxSSELine = 4 << 20

var (
	errStreamEnd   = errors.New("sse done")
	errLineTooLong = errors.New("sse line too long")
)

// readSSE 按 SSE 事件切分。每个事件的 data 行用换行拼成一段文本交给 fn。
// 读到 data: [DONE] 时返回 errStreamEnd。没有 [DONE] 的正常结束返回 nil。
func readSSE(r io.Reader, fn func(payload string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data []string
	first := true
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return errStreamEnd
		}
		return fn(payload)
	}
	handle := func(line string) error {
		if line == "" {
			return flush()
		}
		if strings.HasPrefix(line, ":") {
			return nil
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			field = line
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field == "data" {
			data = append(data, value)
		}
		return nil
	}
	for {
		line, err := readSSELine(br, maxSSELine)
		if errors.Is(err, errLineTooLong) {
			return errLineTooLong
		}
		if first {
			line = strings.TrimPrefix(line, "\uFEFF")
			first = false
		}
		if err == nil || line != "" {
			if herr := handle(line); herr != nil {
				return herr
			}
		}
		if errors.Is(err, io.EOF) {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func readSSELine(r *bufio.Reader, max int) (string, error) {
	var b strings.Builder
	for {
		frag, err := r.ReadSlice('\n')
		if len(frag) > 0 {
			if b.Len()+len(frag) > max {
				return "", errLineTooLong
			}
			b.Write(frag)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		text := strings.TrimRight(b.String(), "\r\n")
		if err != nil {
			return text, err
		}
		return text, nil
	}
}
