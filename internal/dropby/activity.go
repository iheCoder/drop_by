package dropby

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ActivitySummary 只为界面解释已经存在的工作或工具记录，不保存另一套进度。
// Title/Detail 表达对象与结果，Status 表达这条记录的含义，SourceID 回到原约定。
// 完整参数、错误仍通过原始记录查看；模型不参与改写这些执行事实。
type ActivitySummary struct {
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Status   string `json:"status"`
	SourceID string `json:"source_id,omitempty"`
}

// 内部调用与引用不直接充当用户文案。遇到此类执行指令时使用真实来源原话，
// 不尝试删掉几个单词后拼出一段可能改变原意的“摘要”。
var internalActivityText = regexp.MustCompile(`\b(?:work|member|msg|tool|responsibility|coordination)_[a-zA-Z0-9_]+\b|\b(?:read_work|delegate_work|close_responsibility|cancel_work|schedule_followup|schedule_exploration|read_tool_result|save_artifact)\b`)

func activityExcerpt(text string) string {
	text = strings.TrimSpace(text)
	if text == "" || internalActivityText.MatchString(text) || json.Valid([]byte(text)) {
		return ""
	}
	return clip(text, 200)
}

func (s *Store) activityMember(id string) string {
	m, err := s.Member(id)
	if err != nil {
		return "这位朋友"
	}
	return m.Name
}

func (s *Store) activitySource(room, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	var body string
	err := s.db.QueryRow(`SELECT body FROM messages WHERE conversation_id=? AND id=? AND author_id='user'`, room, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return clip(body, 200), err
}

func workStatusText(w Work) string {
	switch w.Status {
	case "pending":
		if w.Kind == "revisit" {
			return "稍后再看"
		}
		return "已安排"
	case "running":
		return "进行中"
	case "waiting_budget":
		return "等待额度恢复"
	case "done":
		return "已结束"
	case "failed":
		return "未完成"
	case "paused":
		return "已暂停"
	case "stopped":
		return "已停止"
	case "superseded":
		return "已随新消息调整"
	}
	return "等待更新"
}

func (s *Store) workSummary(w Work) (ActivitySummary, error) {
	d := ActivitySummary{Title: "后续安排", Status: workStatusText(w), SourceID: w.SourceID}
	name := s.activityMember(w.MemberID)
	switch w.Kind {
	case "research":
		d.Title = name + "的探索"
		if w.AssignedBy != "" {
			d.Title = s.activityMember(w.AssignedBy) + "请" + name + "协作"
		}
	case "revisit":
		d.Title = name + "想稍后再看看"
	case "share":
		d.Title = name + "的结果分享"
	default:
		d.Title = "这次回应"
	}
	if strings.HasPrefix(w.ID, "coordination_") {
		d.Title = name + "跟进同伴的结果"
	}
	if w.Kind == "research" || (w.Kind == "revisit" && !strings.HasPrefix(w.ID, "coordination_")) {
		d.Detail = activityExcerpt(w.Instruction)
	}
	if d.Detail == "" {
		var err error
		d.Detail, err = s.activitySource(w.ConversationID, w.SourceID)
		if err != nil {
			return d, err
		}
	}
	// 结果描述只在实际结束后展示，失败的错误正文留在技术详情。
	if w.Status == "done" {
		if result := activityExcerpt(w.Result); result != "" {
			d.Detail = result
		}
	}
	return d, nil
}

var actionTitles = map[string]string{
	"delegate_work": "安排朋友协作", "close_responsibility": "结束临时分工", "cancel_work": "取消后续安排",
	"search": "搜索网页", "read_page": "阅读网页", "save_artifact": "保存成果", "remember": "整理经历",
	"schedule_exploration": "安排探索", "schedule_followup": "留个后续", "ask_question": "提出问题",
	"send_private": "发起私聊", "read_history": "回顾聊天", "read_artifact": "查看成果",
	"read_work": "查看进展", "read_tool_result": "核对材料", "read_memory": "回顾经历", "search_memory": "寻找经历",
}

func (s *Store) actionSummary(run ToolRun) (ActivitySummary, error) {
	d := ActivitySummary{Title: actionTitles[run.Name]}
	var cached struct {
		Reused bool `json:"reused"`
	}
	if json.Unmarshal([]byte(run.Result), &cached) == nil && cached.Reused {
		if run.Name == "search" {
			d.Title = "查看已有搜索结果"
		}
		if run.Name == "read_page" {
			d.Title = "查看已有网页内容"
		}
	}

	if d.Title == "" {
		d.Title = "查看行动"
	}
	// 工具参数采用下划线字段，单独声明保证 ID 归属正确；只展示有业务含义的参数。
	var args struct {
		ID          string `json:"id"`
		MemberID    string `json:"member_id"`
		Instruction string `json:"instruction"`
		Query       string `json:"query"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		Text        string `json:"text"`
	}
	_ = json.Unmarshal(run.Arguments, &args)
	var result struct {
		WorkID  string `json:"work_id"`
		Status  string `json:"status"`
		Error   string `json:"error"`
		Pending []Work `json:"pending_work"`
	}
	_ = json.Unmarshal([]byte(run.Result), &result)
	source, err := s.ReadWork(run.ConversationID, run.WorkID)
	if err != nil {
		return d, err
	}
	d.SourceID = source.SourceID
	switch run.Status {
	case "running":
		d.Status = "进行中"
	case "succeeded":
		d.Status = "已执行"
	case "waiting":
		d.Status = "等待相关工作"
	case "cancelled":
		d.Status = "已停止"
		d.Detail = "这次操作没有继续，已有记录保留。"
	default:
		d.Status = "未完成"
		d.Detail = "这次操作未完成，可查看技术详情。"
	}
	switch run.Name {
	case "delegate_work", "schedule_exploration", "schedule_followup":
		d.Detail = activityExcerpt(args.Instruction)
		if run.Name == "delegate_work" {
			d.Title = "请" + s.activityMember(args.MemberID) + "协作"
		}
		if run.Status == "succeeded" {
			d.Status = "已安排"
			// 安排成功只证明入队。对方的当前进展从真实工作另读，不从工具成功推断。
			if result.WorkID != "" {
				job, err := s.ReadWork(run.ConversationID, result.WorkID)
				if err != nil {
					return d, err
				}
				d.Detail += "\n当前进展：" + workStatusText(job) + "。"
			}
		}
	case "close_responsibility":
		var body string
		var revoked bool
		err := s.db.QueryRow(`SELECT body,revoked FROM responsibilities WHERE id=? AND conversation_id=?`, args.ID, run.ConversationID).Scan(&body, &revoked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return d, err
		}
		if err == nil {
			d.Detail = activityExcerpt(body)
		}
		if run.Status == "succeeded" {
			d.Status = "已结束分工"
		}
		// 旧错误记录不重写；展示当时未能收尾以及现在是否已经结束，避免历史失败冒充当前状态。
		if run.Status == "waiting" || (run.Status == "failed" && strings.Contains(result.Error, "还有未完研究")) {
			d.Status = "等待相关工作"
			if revoked {
				d.Status = "当时未结束 · 现已结束"
			}
			if len(result.Pending) > 0 {
				names := []string{}
				for _, job := range result.Pending {
					names = append(names, s.activityMember(job.MemberID)+"："+workStatusText(job))
				}
				d.Detail += "\n当时仍有工作未结束（" + strings.Join(names, "；") + "）。"
			} else {
				d.Detail += "\n当时仍有相关工作未结束。"
			}
		}
		if revoked && run.Status != "succeeded" {
			d.Detail += "\n这项分工现在已结束。"
		}
	case "cancel_work":
		d.Status = "未取消"
		if run.Status == "running" {
			d.Status = "正在取消"
		}
		if run.Status == "succeeded" {
			d.Status = "已取消"
			if result.Status == "done" || result.Status == "superseded" {
				d.Status = "无需取消"
			}
		}
		job, err := s.ReadWork(run.ConversationID, args.ID)
		if err == nil {
			d.Title = "取消" + s.activityMember(job.MemberID) + "的后续"
			d.Detail = activityExcerpt(job.Instruction)
			if job.MemberID != run.MemberID {
				d.Detail += "\n这次操作没有取消同伴的工作。"
			}
			d.Detail += "\n当前进展：" + workStatusText(job) + "。"
			if run.Status == "failed" && (job.Status == "done" || job.Status == "stopped") {
				d.Status = "未取消 · 工作已结束"
			}
		}
	case "search":
		d.Detail = activityExcerpt(args.Query)
		if run.Status == "succeeded" {
			d.Status = "已搜索"
			if cached.Reused {
				d.Status = "已查看"
			}
		}
	case "read_page":
		d.Detail = activityExcerpt(args.URL)
		if run.Status == "succeeded" {
			d.Status = "已读取"
			if cached.Reused {
				d.Status = "已查看"
			}
		}
	case "save_artifact":
		d.Detail = activityExcerpt(args.Title)
		if run.Status == "succeeded" {
			d.Status = "已保存"
		}
	case "ask_question":
		d.Detail = activityExcerpt(args.Text)
		if run.Status == "succeeded" {
			d.Status = "已提问"
		}
	case "remember":
		if run.Status == "succeeded" {
			d.Status = "已记录"
		}
	case "send_private":
		if run.Status == "succeeded" {
			d.Status = "已发送"
		}
	default:
		if run.Status == "succeeded" {
			d.Status = "已查看"
		}
	}
	if strings.TrimSpace(d.Detail) == "" {
		d.Detail, err = s.activitySource(run.ConversationID, d.SourceID)
		if err != nil {
			return d, err
		}
	}
	return d, nil
}
