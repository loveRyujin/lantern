package tools

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{"type":"object","additionalProperties":false}`)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	url := "https://lantern.local/tools/" + name + ".json"
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	sch, err := c.Compile(url)
	if err != nil {
		return nil, err
	}
	return sch, nil
}

func validateSchema(sch *jsonschema.Schema, value any) error {
	if sch == nil {
		return fmt.Errorf("tools: 缺少 Schema")
	}
	if err := sch.Validate(value); err != nil {
		return invalidArgs("参数不符合 Schema")
	}
	return nil
}
