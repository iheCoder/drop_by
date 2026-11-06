package dropby

// 公开行动和成员自己的回忆不是同一种可见范围。记忆工具虽然在当前聊天执行，
// 结果仍只能被本人读取，不能通过共享工具日志绕过 read_memory 的成员权限。
func toolResultVisibleTo(run ToolRun, memberID string) bool {
	switch run.Name {
	case "remember", "read_memory", "search_memory", "read_tool_result":
		// read_tool_result 也归本人：它可能复制了一条私有记忆结果；若只保护最初
		// 的记忆工具，另一成员仍能沿着第二层读取日志把原文取出来。
		return run.MemberID == memberID
	default:
		return true
	}
}
