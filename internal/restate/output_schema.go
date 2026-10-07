package restate

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// validateOutputSchema 校验 run 终态 final 是否符合结构化输出契约（JSON Schema）。
// final 须为 JSON 文档（对象/数组/标量）；解析失败或校验失败均报错。
func validateOutputSchema(schemaRaw json.RawMessage, final string) error {
	var schemaDoc any
	if err := json.Unmarshal(schemaRaw, &schemaDoc); err != nil {
		return fmt.Errorf("schema 不是合法 JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("output.json", schemaDoc); err != nil {
		return fmt.Errorf("schema 编译失败: %w", err)
	}
	schema, err := compiler.Compile("output.json")
	if err != nil {
		return fmt.Errorf("schema 编译失败: %w", err)
	}
	var doc any
	if err := json.Unmarshal([]byte(final), &doc); err != nil {
		return fmt.Errorf("final 不是合法 JSON: %w", err)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("final 违反输出契约: %w", err)
	}
	return nil
}
