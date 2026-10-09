package thinruntime

// Conformance 自证入口（M5）：第三方实现用平台一致性套件自证兼容的说明
// 与最小断言入口。测试侧：go test ./test/contract/ 即自证；本包的
// conformance_test 验证 EffectKey/Ledger 语义与平台同构。
const ConformanceNote = "go test ./test/contract/ —— 协议帧级一致性（delta 序/tool_call/done 唯一终态）"
