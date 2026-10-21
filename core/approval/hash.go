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

// ErrInvalidParams 表示参数不是单个合法 JSON 值，无法做规范化哈希。
// 同一对象里的重复键（包括嵌套对象）也返回这个错误，不能只留下最后一个值。
var ErrInvalidParams = errors.New("approval: 参数快照不是合法 JSON")

// ParamsHash 返回规范化 JSON 的 SHA-256，用小写十六进制表示。
//
// 规范化会去掉空白并按键名排序，嵌套对象同样处理。数组顺序保持不变。
// 数字保留 JSON 里的写法，因此 "1" 和 "1.0" 的哈希不同。
// 字符串按 UTF-8 输出，不把中文转成 \uXXXX，也不把 & < > 转成 \u0026 这类形式。
// 对象里出现重复键时返回 ErrInvalidParams。解码进 map 会悄悄丢掉前面的值，
// 确认弹窗如果展示原始参数，用户看到的就和实际哈希不一致。
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
	v, err := decodeJSONValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: 参数必须是单个 JSON 值", ErrInvalidParams)
	}
	return marshalCanonical(v)
}

// decodeJSONValue 自己走 token，是为了在键进入 map 之前发现重复。
// encoding/json 解码到 map 时只保留最后一个同名键。
func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, wrapJSON(err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		return decodeObject(dec)
	case '[':
		return decodeArray(dec)
	default:
		return nil, fmt.Errorf("%w: 非法的 JSON 分隔符", ErrInvalidParams)
	}
}

func decodeObject(dec *json.Decoder) (map[string]any, error) {
	obj := make(map[string]any)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, wrapJSON(err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("%w: 对象键不是字符串", ErrInvalidParams)
		}
		if _, exists := obj[key]; exists {
			return nil, fmt.Errorf("%w: 重复键 %q", ErrInvalidParams, key)
		}
		val, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		obj[key] = val
	}
	end, err := dec.Token()
	if err != nil {
		return nil, wrapJSON(err)
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("%w: 对象没有正确结束", ErrInvalidParams)
	}
	return obj, nil
}

func decodeArray(dec *json.Decoder) ([]any, error) {
	var arr []any
	for dec.More() {
		val, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, wrapJSON(err)
	}
	if delim, ok := end.(json.Delim); !ok || delim != ']' {
		return nil, fmt.Errorf("%w: 数组没有正确结束", ErrInvalidParams)
	}
	return arr, nil
}

func wrapJSON(err error) error {
	if err == nil || errors.Is(err, ErrInvalidParams) {
		return err
	}
	return fmt.Errorf("%w: %s", ErrInvalidParams, err.Error())
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
