package restate

import (
	"testing"
)

func TestIsOutboundTool(t *testing.T) {
	for _, tool := range []string{"post_message", "slack_send_message", "message.create", "send_chat", "post_chat"} {
		if !isOutboundTool(tool) {
			t.Errorf("%s 应识别为出站工具", tool)
		}
	}
	for _, tool := range []string{"bash", "read_file", "write_file", "get_weather"} {
		if isOutboundTool(tool) {
			t.Errorf("%s 不应识别为出站工具", tool)
		}
	}
}
