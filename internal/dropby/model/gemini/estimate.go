package gemini

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode"
)

// 只为当前文本请求估算容量。解开 JSON 转义再衡量内容，避免把中文 UTF-8
// 和转义符重复算成 token。对标点、非 ASCII、编码签名和结构保留更多空间。
func estimateRequestTokens(raw []byte) (int, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	count := 32
	textTokens := 0
	previous := ""
	for {
		item, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		switch value := item.(type) {
		case string:
			if previous == "thoughtSignature" {
				count += len(value)
				previous = ""
			} else {
				textTokens += estimateTextTokens(value)
				previous = value
			}
		case json.Number:
			count += (len(value) + 1) / 2
			previous = ""
		default:
			previous = ""
		}
		count++ // 字段分隔、消息边界及短字段的额外开销。
	}
	// 用测光中 Hydrogen 的聊天、探索和搜索续跑请求校准文本部分。
	// 小请求的固定开销在样本中覆盖不足，先按原规则估前 2048；
	// 超出的文本取原始估值的 53%。签名和结构开销保持原值。
	const unscaledText = 2048
	count += min(textTokens, unscaledText) + (53*max(0, textTokens-unscaledText)+99)/100
	// 本地分词与供应商内部序列化不同，保留四分之一作为缓冲。
	return count + (count+3)/4, nil
}

func estimateTextTokens(value string) int {
	count, run, width := 0, 0, 0
	for _, r := range value {
		next := 1
		switch {
		case r >= 'a' && r <= 'z':
			next = 3
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			next = 2
		case r == ' ':
			next = 4
		case r > unicode.MaxASCII && !unicode.IsLetter(r) && !unicode.IsSpace(r):
			next = -3 // 表情和其他非 ASCII 符号可能展开到多个 token。
		}
		if next <= 1 || next != width {
			if run > 0 {
				count += (run + width - 1) / width
			}
			run = 0
		}
		if next <= 1 {
			count += max(1, -next)
		} else {
			run++
		}
		width = next
	}
	if run > 0 {
		count += (run + width - 1) / width
	}
	return count
}
