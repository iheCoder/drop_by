package gemini

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"drop_by/internal/dropby/model"
)

type content struct {
	Role  string            `json:"role,omitempty"`
	Parts []json.RawMessage `json:"parts"`
}

func (m *Client) source() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(Protocol+"\x00"+m.Provider+"\x00"+m.BaseURL+"\x00"+m.Model)))
}

func textPart(text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"text": text})
	return raw
}

func (m *Client) history(messages []model.ChatMessage) ([]any, []json.RawMessage, error) {
	source := m.source()
	var system []json.RawMessage
	turns := make([]any, 0, len(messages))
	var parts []json.RawMessage
	var calls map[string]string
	flushResults := func() {
		if len(parts) > 0 {
			turns = append(turns, content{Role: "user", Parts: parts})
			parts = nil
		}
	}
	appendText := func(role, text string) {
		if text != "" {
			turns = append(turns, content{Role: role, Parts: []json.RawMessage{textPart(text)}})
		}
	}
	for _, msg := range messages {
		if msg.Role != "tool" {
			flushResults()
		}
		switch msg.Role {
		case "system", "developer":
			if msg.Content != "" {
				system = append(system, textPart(msg.Content))
			}
		case "assistant":
			if msg.NativeSource == source && len(msg.ResponseItems) == 1 {
				// 原始 Content 作为完整的一轮回传，保留签名的位置和扩展字段。
				turns = append(turns, msg.ResponseItems[0])
				calls = make(map[string]string, len(msg.ToolCalls))
				for _, call := range msg.ToolCalls {
					calls[call.ID] = call.Function.Name
				}
				continue
			}
			calls = nil // 别的接入点的调用转成事实，回执仍带相同的本地 ID。
			appendText("model", msg.Content)
			for _, call := range msg.ToolCalls {
				fact, _ := json.Marshal(call)
				appendText("model", "先前的工具调用："+string(fact))
			}
		case "tool":
			if name, ok := calls[msg.ToolCallID]; ok {
				result, err := functionResponse(msg.ToolCallID, name, msg.Content)
				if err != nil {
					return nil, nil, err
				}
				parts = append(parts, result)
				delete(calls, msg.ToolCallID)
				continue
			}
			flushResults()
			appendText("user", "先前的工具结果（"+msg.ToolCallID+"）："+msg.Content)
		case "user":
			appendText("user", msg.Content)
			for _, text := range model.LegacyUserText(msg.ResponseItems) {
				appendText("user", text)
			}
		default:
			return nil, nil, fmt.Errorf("Gemini 不支持历史角色 %q", msg.Role)
		}
	}
	flushResults()

	return turns, system, nil
}

func functionResponse(id, name, result string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	var response any = json.RawMessage(result)
	if json.Unmarshal([]byte(result), &object) != nil || object == nil {
		// Gemini 要求响应是对象。旧回执可能是文本、数组或标量，完整
		// 放进 output 字段，既保留原值也让函数结果能和 ID 原生配对。
		if !json.Valid([]byte(result)) {
			response = result
		}
		response = map[string]any{"output": response}
	}
	return json.Marshal(map[string]any{"functionResponse": map[string]any{
		"id": id, "name": name, "response": response,
	}})
}
