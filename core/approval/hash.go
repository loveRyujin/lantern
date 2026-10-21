package approval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ErrInvalidParams 表示参数不是单个 JSON 值，无法做规范化哈希。
var ErrInvalidParams = errors.New("approval: 参数快照不是合法 JSON")

// ParamsHash 返回规范化 JSON 的 SHA-256，用小写十六进制表示。
//
// 规范化会去掉空白并按键名排序，嵌套对象同样处理。数组顺序保持不变。
// 数字保留 JSON 里的写法，因此 "1" 和 "1.0" 的哈希不同。
// 字符串按 UTF-8 输出，不把中文转成 \uXXXX，也不把 & < > 转成 \u0026 这类形式。
func ParamsHash(params []byte) (string, error) {
	canon, err := canonicalJSON(params)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// SnapshotMatches 报告 params 的规范化哈希是否等于 hash。
func SnapshotMatches(hash string, params []byte) (bool, error) {
	got, err := ParamsHash(params)
	if err != nil {
		return false, err
	}
	return got == hash, nil
}

func canonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidParams, err.Error())
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: 参数必须是单个 JSON 值", ErrInvalidParams)
	}
	return marshalCanonical(v)
}

func marshalCanonical(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return []byte("null"), nil
	case bool:
		if x {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case string:
		return marshalString(x)
	case json.Number:
		if !validNumber(x.String()) {
			return nil, fmt.Errorf("%w: 非法数字", ErrInvalidParams)
		}
		return []byte(x.String()), nil
	case []any:
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, el := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			b, err := marshalCanonical(el)
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var buf bytes.Buffer
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := marshalString(k)
			if err != nil {
				return nil, err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			vb, err := marshalCanonical(x[k])
			if err != nil {
				return nil, err
			}
			buf.Write(vb)
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil
	default:
		return nil, fmt.Errorf("%w: 不支持的 JSON 类型 %T", ErrInvalidParams, v)
	}
}

func marshalString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func validNumber(s string) bool {
	if s == "" {
		return false
	}
	if s[0] != '-' && (s[0] < '0' || s[0] > '9') {
		return false
	}
	return json.Valid([]byte(s))
}
