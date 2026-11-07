package dropby

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// 旧成果全部是 Markdown；增加一列即可保留旧版本、来源和原始正文。
// 不重新解释历史文本里的 HTML，只有新工具调用明确选择 HTML 才提供网页预览。
func (s *Store) initArtifacts() error {
	var exists int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('artifacts') WHERE name='format'`).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		_, err := s.db.Exec(`ALTER TABLE artifacts ADD COLUMN format TEXT NOT NULL DEFAULT 'markdown'`)
		return err
	}
	return nil
}

// 把公开消息中已经引用的保存记录映射成成果卡。一次批量查询覆盖本页消息，
// 不依赖最近工具窗口；后续安静保存的新版本也不会悄悄替换历史分享的内容。
func (s *Store) attachMessageArtifacts(messages []Message) error {
	ids := make([]string, 0, len(messages))
	positions := map[string]int{}
	for i, message := range messages {
		if message.AuthorID == "user" || message.AuthorID == "system" || len(message.Evidence) == 0 {
			continue
		}
		ids = append(ids, message.ID)
		positions[message.ID] = i
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.Query(`SELECT DISTINCT m.id,a.id,coalesce(a.conversation_id,''),a.member_id,a.title,a.version,a.source_id,a.created_at,a.format
	FROM json_each(?) selected JOIN messages m ON m.id=selected.value
	JOIN json_each(m.evidence) evidence
	JOIN tool_runs t ON t.id=evidence.value AND (t.conversation_id=m.conversation_id OR (t.conversation_id='' AND t.member_id=m.author_id)) AND t.status='succeeded' AND t.name='save_artifact'
	JOIN artifacts a ON a.id=json_extract(CASE WHEN json_valid(t.result) THEN t.result ELSE '{}' END,'$.artifact_id')
	AND a.version=json_extract(CASE WHEN json_valid(t.result) THEN t.result ELSE '{}' END,'$.version') AND (a.conversation_id=m.conversation_id OR (a.conversation_id IS NULL AND a.member_id=m.author_id))
	ORDER BY m.seq,a.created_at,a.id,a.version`, asJSON(ids))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var artifact Artifact
		if err := rows.Scan(&messageID, &artifact.ID, &artifact.ConversationID, &artifact.MemberID, &artifact.Title, &artifact.Version, &artifact.SourceID, &artifact.CreatedAt, &artifact.Format); err != nil {
			return err
		}
		messages[positions[messageID]].Artifacts = append(messages[positions[messageID]].Artifacts, artifact)
	}
	return rows.Err()
}

func (t *Toolbox) saveArtifact(ctx context.Context, c Conversation, m Member, w Work, a Artifact) (any, error) {
	validContent := func() bool {
		return strings.TrimSpace(a.Title) != "" && strings.TrimSpace(a.Content) != ""
	}
	if !validContent() {
		return nil, fmt.Errorf("成果需要有效标题和完整内容")
	}
	// 编辑时省略格式就沿用原作，避免一次文字修改把可预览的 HTML 变成源码笔记。
	// 事务内仍重新校验所属会话及版本，以防迟到的生成覆盖另一个聊天的成果。
	if a.Format == "" && a.ID != "" {
		var room, owner string
		if err := t.Store.db.QueryRow(`SELECT coalesce(conversation_id,''),member_id,format FROM artifacts WHERE id=? ORDER BY version DESC LIMIT 1`, a.ID).Scan(&room, &owner, &a.Format); err != nil {
			return nil, fmt.Errorf("找不到要修改的成果")
		}
		if room != c.ID || (room == "" && owner != m.ID) {
			return nil, fmt.Errorf("只能修改当前聊天的成果")
		}
	}
	if a.Format == "" {
		a.Format = "markdown"
	}
	if a.Format != "markdown" && a.Format != "html" {
		return nil, fmt.Errorf("成果格式只支持 markdown 或 html")
	}
	newArtifact := a.ID == ""
	if newArtifact {
		a.ID = newID("artifact")
	}
	a.Version, a.ConversationID, a.MemberID = 1, c.ID, m.ID
	a.SourceID, a.CreatedAt = w.SourceID, now()
	err := t.Store.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		if !newArtifact {
			var room, owner string
			if err := tx.QueryRow(`SELECT coalesce(conversation_id,''),member_id,max(version) FROM artifacts WHERE id=? GROUP BY id`, a.ID).Scan(&room, &owner, &a.Version); err != nil {
				return fmt.Errorf("找不到要修改的成果")
			}
			if room != c.ID || (room == "" && owner != m.ID) {
				return fmt.Errorf("只能修改当前聊天的成果")
			}
			a.Version++
		}
		_, err := tx.Exec(`INSERT INTO artifacts(id,version,conversation_id,member_id,title,content,source_id,created_at,format) VALUES(?,?,nullif(?,''),?,?,?,?,?,?)`, a.ID, a.Version, c.ID, m.ID, a.Title, a.Content, a.SourceID, a.CreatedAt, a.Format)
		return err
	})
	// 源码已经在工具参数和成果里保存。返回可验证的版本与入口即可，不把长 HTML
	// 再复制一遍塞进模型上下文；需要核对时可调用 read_artifact。
	base := "/api/artifacts/" + a.ID
	result := map[string]any{"artifact_id": a.ID, "title": a.Title, "version": a.Version, "format": a.Format, "url": base, "download_url": base + "/download?version=" + strconv.Itoa(a.Version)}
	if a.Format == "html" {
		result["preview_url"] = base + "/preview?version=" + strconv.Itoa(a.Version)
	}
	return result, err
}

// 预览采用独立的不透明来源。脚本可以操作自己的页面，但不能读取宿主聊天、存储
// 或 API，不能开弹窗、提交表单或导航父页面。CSP 同时限制常见资源与联网 API。
// 浏览器沙箱仍允许页面导航自身，所以不能把这一边界宣传为完全网络隔离。
const artifactPreviewCSP = "sandbox allow-scripts; default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; worker-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

func (a *API) artifactVersion(w http.ResponseWriter, r *http.Request) (Artifact, bool) {
	version := 0
	if raw := r.URL.Query().Get("version"); raw != "" {
		var err error
		version, err = strconv.Atoi(raw)
		if err != nil || version < 1 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "成果版本无效"})
			return Artifact{}, false
		}
	}
	var result Artifact
	err := a.engine.Store.db.QueryRow(`SELECT id,coalesce(conversation_id,''),member_id,title,content,version,source_id,created_at,format FROM artifacts WHERE id=? AND (?=0 OR version=?) ORDER BY version DESC LIMIT 1`, r.PathValue("id"), version, version).
		Scan(&result.ID, &result.ConversationID, &result.MemberID, &result.Title, &result.Content, &result.Version, &result.SourceID, &result.CreatedAt, &result.Format)
	if err == sql.ErrNoRows {
		respond(w, http.StatusNotFound, map[string]string{"error": "成果或版本不存在"})
		return Artifact{}, false
	}
	if err != nil {
		a.fail(w, err)
		return Artifact{}, false
	}
	return result, true
}

func (a *API) artifactPreview(w http.ResponseWriter, r *http.Request) {
	result, ok := a.artifactVersion(w, r)
	if !ok {
		return
	}
	if result.Format != "html" {
		respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "这个版本是 Markdown 成果"})
		return
	}
	w.Header().Set("Content-Security-Policy", artifactPreviewCSP)
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), fullscreen=()")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, result.Content)
}

func (a *API) artifactDownload(w http.ResponseWriter, r *http.Request) {
	result, ok := a.artifactVersion(w, r)
	if !ok {
		return
	}
	extension, contentType := ".md", "text/markdown; charset=utf-8"
	if result.Format == "html" {
		extension, contentType = ".html", "text/html; charset=utf-8"
	}
	// 文件名仅使用系统生成的 ID 和数字版本，不能把模型标题写进 HTTP 头。
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-v%d%s"`, result.ID, result.Version, extension))
	w.Header().Set("Content-Type", contentType)
	io.WriteString(w, result.Content)
}

// 个人作品先属于作者。发到群里的某一版成为共同材料，作者后来安静修改的版本
// 仍属于作者；不能因为公开过第一版就让同伴读到所有未发布草稿。
const visibleArtifactSQL = `(a.conversation_id=? OR (a.conversation_id IS NULL AND a.member_id=?) OR EXISTS(
 SELECT 1 FROM messages message JOIN json_each(message.evidence) evidence
 JOIN tool_runs run ON run.id=evidence.value AND run.name='save_artifact' AND run.status='succeeded'
 WHERE message.conversation_id=? AND message.author_id=a.member_id
 AND json_extract(CASE WHEN json_valid(run.result) THEN run.result ELSE '{}' END,'$.artifact_id')=a.id
 AND json_extract(CASE WHEN json_valid(run.result) THEN run.result ELSE '{}' END,'$.version')=a.version))`

func (s *Store) artifactVisible(room, member string, a Artifact) (bool, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM artifacts a WHERE a.id=? AND a.version=? AND `+visibleArtifactSQL, a.ID, a.Version, room, member, room).Scan(&count)
	return count > 0, err
}
