package tools

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"unicode/utf8"
)

const (
	// MaxArgumentBytes 是单次工具参数的上限。超过就不解析。
	MaxArgumentBytes = 1 << 20
	// MaxJSONDepth 是对象和数组的嵌套上限。
	MaxJSONDepth = 32
)

// parseJSON 在测试里替换，用来证明截断路径没有解析。
var parseJSON = parseJSONValue

func parseJSONValue(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	if !utf8.Valid(raw) {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodeValue(dec, 0)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	return value, nil
}

func decodeValue(dec *json.Decoder, depth int) (any, error) {
	if depth > MaxJSONDepth {
		return nil, invalidArgs("参数嵌套过深")
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		return decodeObject(dec, depth+1)
	case '[':
		return decodeArray(dec, depth+1)
	default:
		return nil, invalidArgs("参数不是合法 JSON")
	}
}

func decodeObject(dec *json.Decoder, depth int) (map[string]any, error) {
	if depth > MaxJSONDepth {
		return nil, invalidArgs("参数嵌套过深")
	}
	obj := make(map[string]any)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, invalidArgs("参数不是合法 JSON")
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, invalidArgs("参数不是合法 JSON")
		}
		if _, exists := obj[key]; exists {
			return nil, invalidArgs("参数不是合法 JSON")
		}
		val, err := decodeValue(dec, depth)
		if err != nil {
			return nil, err
		}
		obj[key] = val
	}
	end, err := dec.Token()
	if err != nil {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	return obj, nil
}

func decodeArray(dec *json.Decoder, depth int) ([]any, error) {
	if depth > MaxJSONDepth {
		return nil, invalidArgs("参数嵌套过深")
	}
	var arr []any
	for dec.More() {
		val, err := decodeValue(dec, depth)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	if delim, ok := end.(json.Delim); !ok || delim != ']' {
		return nil, invalidArgs("参数不是合法 JSON")
	}
	if arr == nil {
		arr = []any{}
	}
	return arr, nil
}

// jsonNumbersOK 拒绝 big.Rat 无法表示的数字（例如 1e400），避免 Schema 库在边界值上崩溃。
func jsonNumbersOK(v any) bool {
	switch t := v.(type) {
	case json.Number:
		_, ok := new(big.Rat).SetString(t.String())
		return ok
	case map[string]any:
		for _, el := range t {
			if !jsonNumbersOK(el) {
				return false
			}
		}
	case []any:
		for _, el := range t {
			if !jsonNumbersOK(el) {
				return false
			}
		}
	}
	return true
}

func finishTruncated(reason string) bool {
	switch reason {
	case "length", "content_filter":
		return true
	default:
		return false
	}
}

// TruncatedFinish 报告这个 finish_reason 是不是截断。
// length 是输出被 max_tokens 截断。content_filter 也视为不完整。
// 流中断由调用方用 Truncated 字段另外标出。
func TruncatedFinish(reason string) bool {
	return finishTruncated(reason)
}
