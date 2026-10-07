package restate

import "strconv"

// StepName 是 Restate Run 步骤名（journal 位置标识；harness:<step> /
// exec:<step>:<toolID>——契约规范 §1：run 内唯一，模型/协议版本由 run 绑定）。
func StepName(kind string, step int, toolID string) string {
	if toolID == "" {
		return kind + ":" + strconv.Itoa(step)
	}
	return kind + ":" + strconv.Itoa(step) + ":" + toolID
}
