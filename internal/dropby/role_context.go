package dropby

// roleWork 是角色需要的事项视图。预算预留、重试次数和调度器报错只留给运行
// 详情，不能随 trigger/read_work 混入角色的日常台词。已完成的业务结果保留。
func roleWork(w Work) Work {
	w.Attempts = 0
	w.BudgetRequired = 0
	if w.Status != "done" || (w.Kind != "research" && w.Kind != "share") {
		w.Result = ""
	}
	// pending 也可能是从故障等待恢复而来；运行结果只用于已完成的工作。
	if w.Status == "pending" {
		w.Result = ""
	}
	if w.Status == "failed" || w.Status == "waiting_budget" || w.Status == "paused" {
		w.Status = "unfinished"
	}
	return w
}
