package dropby

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	Store       *Store
	Model       Model
	Config      Config
	Tools       *Toolbox
	Hub         *Hub
	mu          sync.Mutex
	running     map[string]context.CancelFunc
	activities  map[string]Activity
	wg          sync.WaitGroup
	memberGates sync.Map
}

func NewEngine(s *Store, m Model, c Config) *Engine {
	e := &Engine{Store: s, Model: m, Config: c, Hub: NewHub(), running: map[string]context.CancelFunc{}, activities: map[string]Activity{}}
	e.Tools = &Toolbox{Store: s, Config: c, Hub: e.Hub, Activity: e.setActivity}
	return e
}

func (e *Engine) setActivity(room, member, label string) {
	e.mu.Lock()
	key := room + ":" + member
	if label == "" {
		delete(e.activities, key)
	} else {
		e.activities[key] = Activity{room, member, label}
	}
	e.mu.Unlock()
	e.Hub.Publish(room)
}

// 服务拥有后台工作的生命周期，浏览器的 SSE 连接只负责查看，不控制任务存亡。
func (e *Engine) Run(ctx context.Context) {
	// 启动配置可能把日预算改为周预算，或提高了原额度。让既有预算等待重新
	// 经过真实预留检查，而不是继续被旧的“明天/下周再试”时间挡住；不重置账本。
	if _, err := e.Store.db.Exec(`UPDATE work SET due_at=? WHERE status='waiting_budget'`, time.Now().UnixMilli()); err != nil {
		slog.Error("重新核对预算等待失败", "error", err)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if e.Config.APIKey != "" {
			if err := e.Store.ScheduleExploration(e.Config.ExploreInterval); err != nil {
				slog.Error("安排探索失败", "error", err)
			}
			e.dispatch(ctx)
		}
		select {
		case <-ctx.Done():
			e.mu.Lock()
			for _, cancel := range e.running {
				cancel()
			}
			e.mu.Unlock()
			e.wg.Wait()
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) Submit(m Message) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.Store.AddUserMessage(m, e.Config.ExploreInterval); err != nil {
		return err
	}
	// 只有明确停止立即取消执行；普通补充留给成员接续，不浪费已开始的推理。
	if discussion, research := explicitStop(m.Body); discussion || research {
		if cancel := e.running[m.ConversationID]; cancel != nil {
			cancel()
		}
		if c, err := e.Store.Conversation(m.ConversationID); err == nil {
			for _, member := range c.MemberIDs {
				if cancel := e.running["member:"+member]; cancel != nil {
					cancel()
				}
			}
		}
	}
	e.Hub.Publish(m.ConversationID)
	return nil
}

func (e *Engine) dispatch(ctx context.Context) {
	if err := e.Store.recheckBudgetWait(e.Config); err != nil {
		slog.Error("重新核对额度等待失败", "error", err)
	}
	works, err := e.Store.DueWorks()
	if err != nil {
		slog.Error("读取后台工作失败", "error", err)
		return
	}
	for _, w := range works {
		key := w.ConversationID
		if key == "" || directedWork(w) {
			key = "member:" + w.MemberID
		}
		e.mu.Lock()
		if _, ok := e.running[key]; ok {
			e.mu.Unlock()
			continue
		}
		claimed, err := e.Store.Claim(w.ID)
		if err != nil || !claimed {
			e.mu.Unlock()
			continue
		}
		workCtx, cancel := context.WithCancel(ctx)
		e.running[key] = cancel
		e.wg.Add(1)
		e.mu.Unlock()
		e.Hub.Publish(w.ConversationID)
		go func(w Work) {
			defer e.wg.Done()
			defer cancel()
			err := e.process(workCtx, w)
			status, result, due := "done", "", time.Now()
			if err != nil {
				result = err.Error()
				// 复用已有工作结果携带本次重试的原因。即使后来等待预算或服务
				// 重启，也不能再把原消息里的旧性格设定覆盖回用户的新编辑。
				if strings.HasPrefix(w.Result, identityRevisionRetry) {
					result = identityRevisionRetry + "\n" + result
				}
				switch {
				case errors.Is(err, errActionYield):
					status, due = "pending", time.Now().Add(time.Second)
				case errors.Is(err, context.Canceled):
					if ctx.Err() != nil || w.ConversationID == "" || w.Kind == "research" || w.Kind == "revisit" {
						status = "pending"
						due = time.Now().Add(time.Second)
					} else {
						status = "superseded"
					}
				case errors.Is(err, ErrStale):
					if ctx.Err() != nil || w.ConversationID == "" || w.Kind == "research" || w.Kind == "revisit" {
						status, due = "pending", time.Now().Add(time.Second)
						break
					}
					// 人物或群职责变化都可能使上下文过时，原真人消息仍须处理。
					// 只有资料更新事务留下的记录才允许忽略被后来编辑覆盖的旧偏好。
					retry, retryReason, retryErr := e.reconsiderAfterStale(w)
					if retryErr != nil {
						status, result = "failed", retryErr.Error()
					} else if retry {
						status, due = "pending", time.Now().Add(time.Second)
						result = retryReason
					} else {
						status = "superseded"
					}
				case recoverableNetwork(err):
					status, due = "pending", time.Now().Add(e.Config.ExploreInterval)
				case errors.Is(err, ErrBudget):
					var rejection *budgetRejection
					if errors.As(err, &rejection) {
						w.BudgetRequired = rejection.required
					}
					status = "waiting_budget"
					due = nextBudgetWeek()
				default:
					status = "failed"
				}
			}
			if finishErr := e.Store.Finish(w, status, result, due); finishErr != nil {
				slog.Error("保存工作状态失败", "error", finishErr)
			}

			// 失败原因和已有结果留在行动记录；前端显示可恢复的提示，不把内部报错
			// 伪装成群里某个人说的话，也不要求用户为内部上下文膨胀缩短自己的消息。
			e.mu.Lock()
			delete(e.running, key)
			e.mu.Unlock()
			e.Hub.Publish(w.ConversationID)
		}(w)
	}
}

// 这是既有 Work.Result 中可读的重试说明，同时标识当前人物资料晚于触发消息。
// 不按 attempts 粗略跳过偏好：首次就遇到预算不足的工作，恢复后仍须理解用户设定。
const identityRevisionRetry = "人物设定已更新；沿用最新人物说明继续原消息。"

// 用户停止与资料编辑可使执行失效；普通消息不会撤销运行中的机会。
// Finish 只更新 running，因此已被显式停止或替代的工作不会被重新复活。
func (e *Engine) reconsiderAfterStale(w Work) (bool, string, error) {
	var status, latestHuman, savedResult string
	err := e.Store.db.QueryRow(`SELECT status,coalesce((SELECT id FROM messages WHERE conversation_id=? AND author_id='user' ORDER BY seq DESC LIMIT 1),''),result FROM work WHERE id=?`, w.ConversationID, w.ID).Scan(&status, &latestHuman, &savedResult)
	if err != nil {
		return false, "", err
	}
	if status != "running" {
		return false, "", nil
	}
	reason := "聊天设置已变化；结合最新设置继续原消息。"
	if strings.HasPrefix(savedResult, identityRevisionRetry) {
		reason = identityRevisionRetry
	}
	if w.Kind == "chat" {
		return w.SourceID != "" && latestHuman == w.SourceID, reason, nil
	}
	// 分享/自主观察没有新的真人问题作为替代依据；若仍在运行，就按新身份重新看。
	return w.Kind == "share" || w.Kind == "explore" || w.Kind == "revisit", reason, nil
}

type Control struct {
	receiptID             string
	personalWorkID        string
	ResponsibilityChanges []ResponsibilityChange `json:"responsibility_changes"`
	RelatedWorkID         string                 `json:"related_work_id"`
	PreferenceUpdates     []PreferenceUpdate     `json:"preference_updates"`
	Discussion            string                 `json:"discussion"`
	Research              string                 `json:"research"`
	Guidance              string                 `json:"guidance"`
	StopIDs               []string               `json:"stop_ids"`
	CompleteIDs           []string               `json:"complete_ids"`
	ResumeIDs             []string               `json:"resume_ids"`
	Updates               []struct {
		ID          string `json:"id"`
		Instruction string `json:"instruction"`
	} `json:"updates"`
}

// applyControl 落实成员明确提交的用户指令，不再调用全群共用解释模型。
// 职责、暂停和工作仍保存在原有表中，其他成员读取同一份实际状态。
func (e *Engine) applyControl(ctx context.Context, c Conversation, w Work, requested ...Control) error {
	control := Control{Discussion: "keep", Research: "keep", Guidance: c.Guidance}
	if len(requested) > 0 {
		control = requested[0]
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(control.PreferenceUpdates) > 0 {
		var saved string
		if err := e.Store.db.QueryRow(`SELECT result FROM work WHERE id=?`, w.ID).Scan(&saved); err != nil && err != sql.ErrNoRows {
			return err
		}
		if strings.HasPrefix(saved, identityRevisionRetry) {
			control.PreferenceUpdates = nil
		}
	}
	return e.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if err := applyResponsibilities(tx, c, w, control.ResponsibilityChanges); err != nil {
			return err
		}
		if control.RelatedWorkID != "" && control.RelatedWorkID != w.ID {
			var present int
			if err := tx.QueryRow(`SELECT count(*) FROM work WHERE id=? AND conversation_id=?`, control.RelatedWorkID, c.ID).Scan(&present); err != nil {
				return err
			}
			if present != 1 {
				return fmt.Errorf("要继续的事项不在当前群中")
			}
			// 关联只能指向既有事项，不能让两个后续彼此成为祖先。
			var cycle int
			if err := tx.QueryRow(`WITH RECURSIVE family(id) AS (SELECT id FROM work WHERE id=? UNION SELECT w.id FROM work w JOIN family f ON w.parent_id=f.id) SELECT count(*) FROM family WHERE id=?`, w.ID, control.RelatedWorkID).Scan(&cycle); err != nil {
				return err
			}
			if cycle > 0 {
				return fmt.Errorf("事项关联不能形成循环")
			}

			if _, err := tx.Exec(`UPDATE work SET parent_id=? WHERE id=?`, control.RelatedWorkID, w.ID); err != nil {
				return err
			}
		}
		// 用户明确提出的持久偏好归到对应成员，不再只留在当前聊天的 guidance。
		if err := applyPreferenceUpdates(tx, c, w.SourceID, control.PreferenceUpdates); err != nil {
			return err
		}
		discussion, research := c.DiscussionPaused, c.ResearchPaused
		if control.Discussion == "pause" {
			discussion = true
		} else if control.Discussion == "resume" {
			discussion = false
		}
		if control.Research == "pause" {
			research = true
		} else if control.Research == "resume" {
			research = false
		}
		// 控制修订只在授权、方向或工作发生真实变化时提升；普通接话不使旧草稿失效。
		if discussion != c.DiscussionPaused || research != c.ResearchPaused || control.Guidance != c.Guidance || len(control.StopIDs)+len(control.CompleteIDs)+len(control.ResumeIDs)+len(control.Updates) > 0 {
			if _, err := tx.Exec(`UPDATE conversations SET revision=revision+1 WHERE id=?`, c.ID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE conversations SET discussion_paused=?,research_paused=?,guidance=? WHERE id=?`, discussion, research, clip(control.Guidance, 5000), c.ID); err != nil {
			return err
		}
		if control.Research == "pause" {
			if _, err := tx.Exec(`UPDATE work SET status='stopped' WHERE conversation_id=? AND kind IN ('research','revisit') AND status IN ('pending','running','waiting_budget','paused')`, c.ID); err != nil {
				return err
			}
		}
		// 明确要求继续是新的真人授权，给予新的执行根；普通插话不会重置旧的自主名额。
		for status, ids := range map[string][]string{"stopped": control.StopIDs, "done": control.CompleteIDs, "pending": control.ResumeIDs} {
			for _, id := range ids {
				if status == "stopped" || status == "done" {
					if id == control.personalWorkID && id != "" {
						if err := stopWorkFamily(tx, "", id); err != nil {
							return err
						}
						continue
					}
					if err := stopWorkFamily(tx, c.ID, id); err != nil {
						return err
					}
					continue
				}
				if _, err := tx.Exec(`UPDATE work SET status=?,due_at=?,attempts=CASE WHEN ?='pending' THEN 0 ELSE attempts END,
root_id=CASE WHEN ?='pending' THEN ? ELSE root_id END,
source_id=CASE WHEN ?='pending' THEN ? ELSE source_id END,
autonomous=CASE WHEN ?='pending' THEN CASE WHEN kind='revisit' THEN 1 ELSE 0 END ELSE autonomous END
WHERE id=? AND conversation_id=? AND kind IN ('research','revisit')`, status, time.Now().UnixMilli(), status, status, w.ID, status, w.SourceID, status, id, c.ID); err != nil {
					return err
				}
			}
		}
		for _, update := range control.Updates {
			if strings.TrimSpace(update.Instruction) == "" {
				continue
			}
			if _, err := tx.Exec(`UPDATE work SET instruction=?,source_id=? WHERE id=? AND conversation_id=? AND kind IN ('research','revisit') AND status IN ('pending','running','waiting_budget','paused')`, update.Instruction, w.SourceID, update.ID, c.ID); err != nil {
				return err
			}
		}
		if control.receiptID != "" {
			_, err := tx.Exec(`UPDATE tool_runs SET status='succeeded',result=? WHERE id=?`, asJSON(map[string]string{"status": "applied", "conversation_id": c.ID}), control.receiptID)
			return err
		}
		return nil
	})
}

func (e *Engine) process(ctx context.Context, w Work) error {
	// 自主来源仅用于用量统计与排队，不再另设消息配额或子预算。
	if autonomousWork(w) {
		w.Autonomous = true
		ctx = context.WithValue(ctx, autonomyKey{}, rootOf(w))
	}
	if w.ConversationID == "" {
		c, err := e.Store.personalConversation(w.MemberID)
		if err != nil {
			return err
		}
		m, err := e.Store.Member(w.MemberID)
		if err != nil {
			return err
		}
		_, err = e.memberAction(ctx, c, m, w)
		return err
	}
	c, err := e.Store.Conversation(w.ConversationID)
	if err != nil {
		return err
	}

	if w.MemberID != "" && !contains(c.MemberIDs, w.MemberID) {
		return nil
	}
	if c.ResearchPaused && (w.Kind == "research" || w.Kind == "revisit") {
		return nil
	}
	if c.DiscussionPaused && w.Kind != "research" && w.Kind != "explore" && w.Kind != "revisit" {
		return nil
	}
	if directedWork(w) {
		m, err := e.Store.Member(w.MemberID)
		if err != nil {
			return err
		}
		_, err = e.memberAction(ctx, c, m, w)
		return err
	}
	return e.converse(ctx, c, w)
}

// 资料页的人物正文就是行动身份，只在系统输入中放一份。公共说明仅交代运行
// 协议；探索方向和投入程度仍由人物与语境决定，不设置共同的好奇心或固定流程。
func memberSystem(m Member, instruction string) ChatMessage {
	return ChatMessage{Role: "system", Content: "你是 " + m.Name + "。\n" + m.Profile + "\n\n" + instruction + `
交流与行动中，依据你的人格特质，结合当前情境和对方的意思，自主决定是否进一步探索、探索什么、深入到什么程度。对于值得探索的事情，使用可用工具去了解、尝试、验证或创造，把实际发现和结果带回后续的判断与行动。
消息通过 send_message 发出，个人活动省略目的地时发给真人私聊。结束行动返回 JSON {"outcome":"实际行动结果"}，它仅作运行记录。运行故障和额度由界面呈现。
工作窗口会延续。需要更新续接内容时，用 organize_context。旧记录可按需回读，以最新现场为准。
personal_focus 是自己的近期关注或打算，state 是界面展示的当前感受或目标；省略沿用，空串清除。state_source_ids 可附来源。
conversation.responsibilities 是当前组织职责，按真人约定用 apply_instruction 记录；工作安排、进展与结束通过相应工具处理。`}
}

func (e *Engine) contextFor(c Conversation, m Member, w Work, afterSeq ...int64) (map[string]any, error) {
	if c.Kind == "personal" {
		input, err := e.personalContext(m, w)
		if err != nil {
			return nil, err
		}
		after := int64(0)
		if len(afterSeq) > 0 {
			after = afterSeq[0]
		}
		fresh, err := e.Store.personalMessages(m.ID, after, w.CreatedAt)
		input["latest_messages"] = fresh
		return input, err
	}
	c.Avatar = "" // 人物和目标留在上下文，图片留在界面与资料接口。
	start, err := e.Store.contextStart(c.ID, m.ID)
	if err != nil {
		return nil, err
	}
	seen, _, err := e.Store.observation(c.ID, m.ID)
	if err != nil {
		return nil, err
	}
	// 未读不能被旧的话题起点或条数窗口跳过。已续接的窗口只追加未交付原文。
	after := min(max(0, start-1), seen)
	if len(afterSeq) > 0 {
		after = afterSeq[0]
	}
	messages, err := e.Store.messagesAfter(c.ID, after, 0, -1)
	if err != nil {
		return nil, err
	}
	canCoordinate := e.Store.CanDelegate(c, m, w)
	people := []map[string]string{{"id": "user", "name": "你（真人）"}}
	for _, id := range c.MemberIDs {
		member, err := e.Store.Member(id)
		if err != nil {
			return nil, err
		}
		person := map[string]string{"id": id, "name": member.Name}
		if canCoordinate && id != m.ID {
			// 协调依据是公开人物设定和当前群的工作；不读取同伴其他会话的私有状态。
			person["profile"] = member.Profile
			person["tools"] = strings.Join(member.Tools, ", ")
		}
		people = append(people, person)
	}
	focus, err := e.Store.personalActivity(m.ID)
	if err != nil {
		return nil, err
	}
	identity := map[string]any{"id": m.ID, "name": m.Name, "profile_revision": m.ProfileRevision, "tools": m.Tools}
	unread := []string{}
	for _, message := range messages {
		if message.Seq > seen {
			unread = append(unread, message.ID)
		}
	}
	// 现场只更新身份、用户控制与本次交付。经历、资料和工作详情由成员主动查找。
	return map[string]any{"time": now(), "local_time": time.Now().Format(time.RFC3339), "self": identity, "people": people, "conversation": c, "latest_messages": messages, "context_start_seq": start, "trigger": map[string]string{"id": w.ID, "kind": w.Kind}, "seen_through_seq": seen, "new_message_ids": unread, "personal_focus": PersonalActivity{Body: focus.Body, UpdatedAt: focus.UpdatedAt}}, nil
}

func explicitStop(body string) (discussion, research bool) {
	switch strings.Trim(body, " \n\t。！!，,.?") {
	case "先别聊", "别聊了", "暂停讨论":
		return true, false
	case "不用研究", "不用研究了", "停止研究":
		return false, true
	}
	return false, false
}

type Outcome struct {
	seen           int64
	ConversationID string   `json:"conversation_id,omitempty"`
	PersonalFocus  *string  `json:"personal_focus,omitempty"`
	Message        string   `json:"message"`
	Outcome        string   `json:"outcome"`
	Notify         bool     `json:"notify"`
	Evidence       []string `json:"evidence"`
	Unfinished     bool     `json:"unfinished"`
	ReplyTo        string   `json:"reply_to,omitempty"`
	State          *string  `json:"state,omitempty"`
	StateSourceIDs []string `json:"state_source_ids,omitempty"`
	ContextStartID *string  `json:"context_start_id,omitempty"`
}

// 同一成员的实际行动和发布共用一把锁；不同成员互不等待。
// 一个成员依然串行。等待者只表示真实到达的行动，不为个性或消息内容评分。
type memberGate struct {
	slot    chan struct{}
	waiting atomic.Int32
	// 与成员行动共用串行锁。按场景保存上次请求的缓存观察，成员之间互不影响。
	requestCaches map[string]*memberCacheState
}

var errActionYield = errors.New("行动已保存，接续新消息")

func (e *Engine) gate(member string) *memberGate {
	value, _ := e.memberGates.LoadOrStore(member, &memberGate{slot: make(chan struct{}, 1)})
	return value.(*memberGate)
}
func (e *Engine) lockMember(ctx context.Context, member string) (func(), error) {
	gate := e.gate(member)
	gate.waiting.Add(1)
	defer gate.waiting.Add(-1)
	select {
	case gate.slot <- struct{}{}:
		return func() { <-gate.slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// 一次性工具/模型调用也遵守成员自己的串行边界。
func (e *Engine) act(ctx context.Context, c Conversation, m Member, w Work, instruction string) (Outcome, error) {
	unlock, err := e.lockMember(ctx, m.ID)
	if err != nil {
		return Outcome{}, err
	}
	defer unlock()
	return e.actLocked(ctx, c, m, w, instruction)
}

func (e *Engine) actLocked(ctx context.Context, c Conversation, m Member, w Work, instruction string) (Outcome, error) {
	// 后续聊天仍使用发言模型。只有已经进入实际研究的工作才用主模型深入执行，
	// 不以“第几句话”决定升级，也不改写成员自己的参与意愿。
	role, purpose := "chat", "chat"
	if w.Kind == "research" || c.Kind == "personal" {
		role, purpose = "", "research"
	}
	ctx = context.WithValue(ctx, modelRoleKey{}, role)
	ctx = withUsageCall(ctx, workUsage(purpose, m.ID, c, w))
	ctx = context.WithValue(ctx, personalSinceKey{}, w.CreatedAt)
	if c.Kind == "personal" && w.Kind == "explore" && w.ID != "" {
		ctx = e.withMemberCache(ctx, m.ID, c.ID, w.ID)
	} else if c.Kind == "direct" && purpose == "chat" {
		// 私聊只对照原本就会发生的请求，用于定位短间隔仍整段重写的原因。
		ctx = e.withMemberCache(ctx, m.ID, c.ID, "")
	}

	// 等待期间可能有人修改了这位成员的偏好，执行时读取当前身份。
	var err error
	m, err = e.Store.Member(m.ID)
	if err != nil {
		return Outcome{}, err
	}
	e.setActivity(c.ID, m.ID, observationLabel(c, w))
	defer e.setActivity(c.ID, m.ID, "")
	// 研究使用更充分的推理；工具能力不按首轮/后续轮限制。
	if w.Kind == "research" || c.Kind == "personal" {
		ctx = context.WithValue(ctx, deepReasoningKey{}, true)
	}
	// 优先恢复未完调用；新行动接续本人的工作窗口，不借用其他成员的理解。
	cp, loadErr := e.Store.loadAction(w.ID, m.ID)
	if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
		return Outcome{}, loadErr
	}
	window, err := e.Store.loadWorkingContext(c.ID, m.ID)
	if err != nil {
		return Outcome{}, err
	}
	var after []int64
	if loadErr == nil {
		after = []int64{cp.Seen}
	} else if len(window.Messages) > 0 {
		after = []int64{window.Seen}
	}
	input, err := e.contextFor(c, m, w, after...)
	if err != nil {
		return Outcome{}, err
	}
	seen := observedThrough(input)
	if len(after) > 0 {
		seen = max(seen, after[0])
	}
	// 用本次实际行动的工具起点识别新委托；旧记录中的安排不能吞掉这次尚未完成的工作。
	var toolStart int64
	if err := e.Store.db.QueryRow(`SELECT coalesce(max(rowid),0) FROM tool_runs WHERE work_id=?`, w.ID).Scan(&toolStart); err != nil {
		return Outcome{}, err
	}
	messages := []ChatMessage{memberSystem(m, instruction), {Role: "user", Content: asJSON(input), PendingDelivery: true}}
	if original, ok := input["latest_messages"].([]Message); ok {
		history := window.Messages
		if loadErr == nil {
			history = cp.Messages
		}
		filtered := withoutDelivered(original, history)
		input["latest_messages"] = filtered
		ids := []string{}
		for _, message := range filtered {
			ids = append(ids, message.ID)
		}
		input["new_message_ids"] = ids
		messages[1].Content = asJSON(input)
	}
	currentSystem, currentInput := messages[0], messages[1]
	var updateAfterTools string
	if loadErr == nil {
		messages = refreshScene(restoreSceneOrder(cp.Messages))
		if len(messages) > 0 && messages[0].Role == "system" {
			messages[0] = currentSystem
		}
		// 有新消息或控制变化，旧调用必须重新判断；没有变化则继续消费断点。
		if seen > cp.Seen || cp.Revision != c.Revision || cp.ProfileRevision != m.ProfileRevision {
			messages, err = e.resumeReceipts(messages, w, m)
			if err != nil {
				return Outcome{}, err
			}
			messages = withScene(messages, currentInput)
			cp.Seen = seen
		} else if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
			updateAfterTools = currentInput.Content
		} else {
			messages = withScene(messages, currentInput)
		}

	} else {
		if len(window.Messages) > 0 {
			messages = refreshScene(window.Messages)
			if messages[0].Role == "system" {
				messages[0] = currentSystem
			} else {
				messages = append([]ChatMessage{currentSystem}, messages...)
			}
			messages = withScene(messages, currentInput)
		}
		// 行动来由只在新行动交付一次；恢复与整理都沿用成员已有的理解。
		arrived, _ := input["latest_messages"].([]Message)
		cause, err := e.actionInput(c, w, arrived, messages, window.Seen)
		if err != nil {
			return Outcome{}, err
		}
		cause.PendingDelivery = true
		messages = append(messages, cause)
		cp = actionCheckpoint{ID: newID("msg"), WorkID: w.ID, MemberID: m.ID, Seen: seen}
	}
	cp.Revision = c.Revision
	cp.ProfileRevision = m.ProfileRevision
	defs := e.Tools.Definitions(m, c, w)
	for {
		if w.ID != "" {
			var status string
			err := e.Store.db.QueryRow(`SELECT status FROM work WHERE id=?`, w.ID).Scan(&status)
			if err != nil && err != sql.ErrNoRows {
				return Outcome{}, err
			}
			if status == "stopped" || status == "superseded" {
				return Outcome{}, errActionStopped
			}
		}
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		if w.Kind == "research" {
			e.setActivity(c.ID, m.ID, "正在整理研究材料")
		} else {
			e.setActivity(c.ID, m.ID, observationLabel(c, w))
		}
		messages = distinctMessageSources(messages)
		cp.Messages = messages
		if err := e.Store.saveAction(cp); err != nil {
			return Outcome{}, err
		}
		// 当前调用结束且断点落盘后交接给已在等待的聊天；不取消在途写操作。
		if (c.ID == "" || directedWork(w)) && e.gate(m.ID).waiting.Load() > 0 {
			return Outcome{}, errActionYield
		}
		// 模型响应先落盘再执行工具。恢复时若末项是响应，就继续消费它；
		// 若末项是工具结果，则只请求下一步，不重复之前的写操作。
		var r ChatMessage
		if len(messages) > 0 && messages[len(messages)-1].Role == "assistant" {
			r = messages[len(messages)-1]
		} else {
			// 已成功压缩的窗口立即保存，不依赖随后生成成功。
			modelCtx := context.WithValue(ctx, preparedContextKey{}, savePreparedContext(func(prepared []ChatMessage) error {
				cp.Messages, cp.Seen = prepared, seen
				return e.Store.saveAction(cp)
			}))
			r, err = e.completeMemberModel(modelCtx, c, &seen, &messages, defs)
			if err != nil {
				return Outcome{}, err
			}
			// 只有正常生成成功，待交付内容才可以在后续轮次进入旧历史。
			for i := range messages {
				messages[i].PendingDelivery = false
			}
			messages = append(messages, r)
			cp.Messages, cp.Seen = messages, seen
			err = e.Store.saveAction(cp)
		}
		if err != nil {
			return Outcome{}, err
		}
		if len(r.ToolCalls) == 0 {
			var out Outcome
			if err = parseJSON(r.Content, &out); err != nil {
				// 旧现场可能已生成完整聊天正文。它仍是成员自己的发言，无需
				// 再叫一个模型重写；损坏的 JSON 则保留为本人的协议错误。
				text := strings.TrimSpace(r.Content)
				if text == "" || strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") || strings.HasPrefix(text, "```") {
					return out, err
				}
				out.Message = text
			}
			evidence := out.Evidence[:0]
			for _, id := range out.Evidence {
				var count int
				if err = e.Store.db.QueryRow(`SELECT count(*) FROM tool_runs WHERE id=? AND (conversation_id=? OR member_id=?)`, id, c.ID, m.ID).Scan(&count); err != nil {
					return out, err
				}
				if count > 0 {
					evidence = append(evidence, id)
				}
			}
			out.Evidence = evidence
			if out.Unfinished {
				var delegated int
				if err := e.Store.db.QueryRow(`SELECT count(*) FROM tool_runs r JOIN work child ON child.id=json_extract(r.result,'$.work_id') WHERE r.work_id=? AND r.rowid>? AND r.status='succeeded' AND r.name IN ('schedule_exploration','schedule_followup','delegate_work') AND child.status IN ('pending','running','waiting_budget')`, w.ID, toolStart).Scan(&delegated); err != nil {
					return out, err
				}
				if delegated > 0 {
					out.Unfinished = false
				}
			}
			out.seen = seen
			if c.ID != "" {
				if err = e.Store.recordObservation(c, m.ID, seen, out.State, out.StateSourceIDs); err != nil {
					return out, err
				}
				if err = e.Store.rememberContextStart(c, m.ID, seen, out.ContextStartID); err != nil {
					return out, err
				}
			}
			if err = e.Store.updatePersonalActivity(m.ID, out.PersonalFocus); err != nil {
				return out, err
			}
			cp.Messages, cp.Seen = messages, seen
			if err = e.Store.saveAction(cp); err != nil {
				return out, err
			}
			return out, nil
		}
		batchStart := len(messages) - 1
		var organization *organizedContext
		var organizationReceipt organizedReceipt
		for _, call := range r.ToolCalls {
			result, err := e.Tools.Execute(ctx, c, m, w, call)
			if errors.Is(err, errActionStopped) || errors.Is(err, ErrBudget) || errors.Is(err, ErrStale) || errors.Is(err, context.Canceled) {
				return Outcome{}, err
			}
			if err != nil {
				result = asJSON(map[string]string{"error": err.Error(), "status": "failed"})
			}
			messages = append(messages, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result, PendingDelivery: true})
			if call.Function.Name == "apply_instruction" && err == nil && c.ID != "" {
				c, err = e.Store.Conversation(c.ID)
				if err != nil {
					return Outcome{}, err
				}
				cp.Revision = c.Revision
				updated, readErr := e.Store.Member(m.ID)
				if readErr != nil {
					return Outcome{}, readErr
				}
				m = updated
				cp.ProfileRevision = m.ProfileRevision
				fresh, readErr := e.contextFor(c, m, w, seen)
				if readErr != nil {
					return Outcome{}, readErr
				}
				// 同批整理也必须沿用新资料，不能重用行动开始时的旧 input。
				input = fresh
				currentInput = ChatMessage{Role: "user", Content: asJSON(fresh)}
				// 工具结果先完整配对，再交付新现场；同时刷新唯一的人物正文，
				// 同批整理和故障恢复都不能带回修改前的人物。
				currentSystem = memberSystem(m, instruction)
				messages[0] = currentSystem
				updateAfterTools = currentInput.Content
				defs = e.Tools.Definitions(m, c, w)
			}
			if call.Function.Name == "organize_context" {
				var receipt struct {
					ID     string           `json:"tool_run_id"`
					Status string           `json:"status"`
					Result organizedContext `json:"result"`
				}
				if parseJSON(result, &receipt) == nil && receipt.Status == "succeeded" {
					organization = &receipt.Result
					// 回执确认这次替换已执行。便笺正文只交付一份。
					organizationReceipt = organizedReceipt{Name: call.Function.Name, CallID: call.ID, Result: asJSON(map[string]string{
						"tool_run_id": receipt.ID, "status": receipt.Status, "work_id": w.ID,
					})}
				}
			}
		}
		deliveredBeforeOrganization := messages
		// 一批工具全部执行并配对后才能切换窗口；同时调用的发言或保存结果不能丢失。
		if organization != nil {
			// 整理只覆盖模型已经看过的窗口。不重新查询记忆、资料或行动来由。
			fresh := make(map[string]any, len(input))
			for key, value := range input {
				fresh[key] = value
			}
			fresh["latest_messages"], fresh["new_message_ids"] = []Message{}, []string{}
			chatAction, err := e.Store.organizedChatAction(w, m.ID, organization.Messages)
			if err != nil {
				return Outcome{}, err
			}
			if chatAction.Content != "" {
				// 当前真人请求已经在本次行动或所选原话中，去掉现场快照的副本。
				incoming, _ := input["latest_messages"].([]Message)
				if room, ok := fresh["conversation"].(Conversation); ok && slices.ContainsFunc(incoming, func(message Message) bool {
					return message.ID == w.SourceID && message.Seq == room.LastSeq && message.Body == room.LastMessage && message.Body == w.Instruction
				}) {
					room.LastMessage = ""
					fresh["conversation"] = room
				}
			}
			// 同批结果是模型尚未见过的新输入，完整交付实际读取范围和写入回执。
			receipts := []organizedReceipt{organizationReceipt}
			for _, item := range messages[batchStart+1:] {
				if item.Role != "tool" {
					continue
				}
				for _, call := range r.ToolCalls {
					if call.ID != item.ToolCallID || call.Function.Name == "organize_context" {
						continue
					}
					receipts = append(receipts, organizedReceipt{Name: call.Function.Name, CallID: call.ID, Result: item.Content})
				}
			}
			messages = []ChatMessage{currentSystem, {Role: "user", Content: asJSON(fresh)}, {Role: "user", Content: organizedContextPrefix + asJSON(organization), PendingDelivery: true}}
			messages = append(messages, ChatMessage{Role: "user", Content: organizedReceiptsPrefix + asJSON(receipts), PendingDelivery: true})
			if chatAction.Content != "" {
				messages = append(messages, chatAction)
			}
		}

		if updateAfterTools != "" {
			messages = withScene(messages, ChatMessage{Role: "user", Content: updateAfterTools, PendingDelivery: true})
			updateAfterTools = ""
		}
		// 普通消息在下一次模型请求前增量交付，不撤回本轮已形成的工具调用或发言。
		{
			var arrived []Message
			var readErr error
			if c.ID == "" {
				arrived, readErr = e.Store.personalMessages(m.ID, seen, w.CreatedAt)
			} else {
				arrived, readErr = e.Store.messagesAfter(c.ID, seen, 0, -1)
			}
			if readErr != nil {
				return Outcome{}, readErr
			}
			if len(arrived) > 0 {
				// 同批整理可能已把发送参数换成回执；两种载体都表示实际交付，
				// 本人已发送的原文无需再作为一条新输入回流。
				fresh := withoutDelivered(withoutDelivered(arrived, deliveredBeforeOrganization), messages)
				if len(fresh) > 0 {
					messages = append(messages, ChatMessage{Role: "user", Content: "接续收到的聊天原文（author_id 标明说话者）：" + asJSON(fresh), PendingDelivery: true})
				}
				seen = arrived[len(arrived)-1].Seq
			}
		}
		cp.Seen = seen
	}
}

// Hub 不存聊天数据。重连时前端从 SQLite 重新读消息，所以丢失一次通知也不会丢消息。
type Hub struct {
	mu          sync.Mutex
	subscribers map[chan string]struct{}
	reactions   map[chan string]struct{}
}

func NewHub() *Hub {
	return &Hub{subscribers: map[chan string]struct{}{}, reactions: map[chan string]struct{}{}}
}
func (h *Hub) Subscribe() (chan string, func()) {
	return h.subscribe(h.subscribers)
}

// 表情只通知网页。群聊调度和行动中的真人补充仍监听原有消息事件。
func (h *Hub) SubscribeReactions() (chan string, func()) {
	return h.subscribe(h.reactions)
}
func (h *Hub) subscribe(subscribers map[chan string]struct{}) (chan string, func()) {
	c := make(chan string, 16)
	h.mu.Lock()
	subscribers[c] = struct{}{}
	h.mu.Unlock()
	return c, func() { h.mu.Lock(); delete(subscribers, c); h.mu.Unlock() }
}
func (h *Hub) Publish(room string) {
	h.publish(h.subscribers, room)
}
func (h *Hub) PublishReactions(room string) {
	h.publish(h.reactions, room)
}
func (h *Hub) publish(subscribers map[chan string]struct{}, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range subscribers {
		select {
		case c <- room:
		default:
		}
	}
}
