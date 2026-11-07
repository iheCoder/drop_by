package dropby

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// 长期经历有独立的小窗口。它不从当前任务、最新原话、人物或工作进度里借空间，
// 不随档案条数增长。需要更多细节时，成员用 read_memory/read_history 主动回查。
const memoryContextChars = 3600

// 中文相邻字对与英文单词提供本地候选检索，无网络费用。这里的 BM25 只衡量
// 文本相关性，不衡量好感、人格或证据可信度；换说法仍可由成员换词主动检索。
// 不把人物设定拼进查询，否则“我喜欢设计”会让每个话题都召回旧设计经历。
func memoryTerms(text string) map[string]int {
	terms := map[string]int{}
	var word []rune
	var han rune
	flush := func() {
		// 时间、数量等裸数字不是个人经历的主题。否则当前的 21:10 会
		// 找回第 21 条无关往事；数字仍保留在正文，主动 search_memory 可查。
		hasLetter := false
		for _, r := range word {
			hasLetter = hasLetter || unicode.IsLetter(r)
		}
		if len(word) > 1 && hasLetter {
			terms[string(word)]++
		}
		word = nil
	}
	for _, r := range strings.ToLower(text) {
		if unicode.Is(unicode.Han, r) {
			flush()
			if han != 0 {
				terms[string([]rune{han, r})]++
			}
			han = r
			continue
		}
		han = 0
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	return terms
}

func (s *Store) relevantMemories(member, room, query string) ([]Memory, error) {
	result := []Memory{}
	queryTerms := memoryTerms(query)
	if len(queryTerms) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(memorySelect+` WHERE m.member_id=? AND (m.conversation_id=? OR m.conversation_id='')`, member, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type candidate struct {
		memory Memory
		terms  map[string]int
		length int
		score  float64
	}
	items := []candidate{}
	frequency := map[string]int{}
	totalLength := 0
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		terms := memoryTerms(m.Body)
		length := 0
		for term, count := range terms {
			length += count
			if queryTerms[term] > 0 {
				frequency[term]++
			}
		}
		totalLength += length
		items = append(items, candidate{memory: m, terms: terms, length: length})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return result, nil
	}
	average := max(1, float64(totalLength)/float64(len(items)))
	for i := range items {
		item := &items[i]
		for term := range queryTerms {
			if count := item.terms[term]; count > 0 {
				idf := math.Log(1 + (float64(len(items)-frequency[term])+0.5)/(float64(frequency[term])+0.5))
				tf := float64(count)
				item.score += idf * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(item.length)/average))
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].score == items[j].score {
			if items[i].memory.UpdatedAt == items[j].memory.UpdatedAt {
				return items[i].memory.ID < items[j].memory.ID
			}
			return items[i].memory.UpdatedAt > items[j].memory.UpdatedAt
		}
		return items[i].score > items[j].score
	})
	for _, item := range items {
		if item.score == 0 || len(result) == 6 {
			break
		}
		m := item.memory
		m.Body = contextExcerpt(m.Body, 600)
		// 连元数据也计入容量，避免很多来源 ID 或长字段绕过正文长度限制。
		proposed := append(append([]Memory{}, result...), m)
		if len([]rune(asJSON(proposed))) <= memoryContextChars {
			result = proposed
		}
	}
	return result, nil
}

func memoryQuery(w Work, trigger *Message, messages []Message) string {
	parts := []string{w.Instruction}
	if trigger != nil {
		parts = append(parts, trigger.Body)
	}
	// 新近现场帮助理解“那个”“上次”，但不把整个聊天窗口作为检索条件，
	// 以免刚结束的话题和多位成员的重复发言挤掉当前事项。
	for _, message := range messages[max(0, len(messages)-3):] {
		parts = append(parts, contextExcerpt(message.Body, 600))
	}
	return strings.Join(parts, "\n")
}
