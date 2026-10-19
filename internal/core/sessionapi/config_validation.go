package sessionapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var applicationAlias = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (c AgentConfig) ValidateApplication() error {
	if c.MaxOutputTokens < 0 || c.MaxSteps < 0 || c.MaxDurationMS < 0 {
		return fmt.Errorf("execution limits cannot be negative")
	}
	if c.MaxDurationMS > int64((1<<63-1)/time.Millisecond) {
		return fmt.Errorf("max_duration_ms exceeds duration range")
	}
	if !c.ApplicationProfile {
		return nil
	}
	if c.ApplicationBridgeRef == "" || len(c.ApplicationTools) == 0 {
		return fmt.Errorf("application profile requires bridge ref and tools")
	}
	aliases, refs := map[string]bool{}, map[string]bool{}
	for _, tool := range c.ApplicationTools {
		if !applicationAlias.MatchString(tool.ModelAlias) || tool.ToolRef == "" || strings.TrimSpace(tool.Description) == "" || aliases[tool.ModelAlias] || refs[tool.ToolRef] {
			return fmt.Errorf("invalid or duplicate application tool %q", tool.ModelAlias)
		}
		if _, err := CompileApplicationSchema(tool.InputSchema); err != nil {
			return fmt.Errorf("tool %q: %w", tool.ModelAlias, err)
		}
		aliases[tool.ModelAlias], refs[tool.ToolRef] = true, true
	}
	return nil
}

func CompileApplicationSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc["type"] != "object" {
		return nil, fmt.Errorf("application tool requires an object input schema")
	}
	var localReferences func(any) bool
	localReferences = func(value any) bool {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if key == "$ref" || key == "$dynamicRef" {
					ref, ok := child.(string)
					if !ok || !strings.HasPrefix(ref, "#") {
						return false
					}
				}
				if !localReferences(child) {
					return false
				}
			}
		case []any:
			for _, child := range v {
				if !localReferences(child) {
					return false
				}
			}
		}
		return true
	}
	if !localReferences(doc) {
		return nil, fmt.Errorf("application schema references must be local")
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("application.json", doc); err != nil {
		return nil, err
	}
	return compiler.Compile("application.json")
}
