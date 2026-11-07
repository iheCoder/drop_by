package dropby

import (
	"fmt"
	"strings"
)

// 省略时使用默认页长；调用者显式给出的数量不再被压成固定上限。
func readDefault(values []int, index, fallback int) int {
	if len(values) > index && values[index] > 0 {
		return values[index]
	}
	return fallback
}

// 从持久记录检索资料，不依赖默认上下文最近几条的窗口。先应用原有可见性，
// 再分页，避免私有记忆占用页位或通过资料目录暴露。
func (t *Toolbox) toolIndex(room, member, query string, offset, limit int) (any, error) {
	if offset < 0 {
		return nil, fmt.Errorf("读取位置无效")
	}
	if limit == 0 {
		limit = 12
	}
	rows, err := t.Store.db.Query(`SELECT id,member_id,name,arguments,result,created_at,status FROM tool_runs WHERE conversation_id=? AND (instr(arguments,?)>0 OR instr(result,?)>0) ORDER BY rowid DESC`, room, query, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	skipped := 0
	more := false
	for rows.Next() {
		var run ToolRun
		var arguments string
		if err := rows.Scan(&run.ID, &run.MemberID, &run.Name, &arguments, &run.Result, &run.CreatedAt, &run.Status); err != nil {
			return nil, err
		}
		if !toolResultVisibleTo(run, member) || run.Name == "read_tool_result" {
			continue
		}
		if skipped < offset {
			skipped++
			continue
		}
		if len(items) == limit {
			more = true
			break
		}
		// 索引只负责发现入口；正文由同一工具按 id、query、offset、length 回读。
		items = append(items, map[string]any{"id": run.ID, "member_id": run.MemberID, "name": run.Name, "arguments": arguments, "created_at": run.CreatedAt, "status": run.Status, "result": sharedToolResult(strings.TrimSpace(run.Result))})
	}
	return map[string]any{"records": items, "next_offset": offset + len(items), "has_more": more}, rows.Err()
}
