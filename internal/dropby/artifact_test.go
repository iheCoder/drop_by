package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestArtifactCardsRequirePublicEvidenceAndPinSharedVersion(t *testing.T) {
	// 成员可以先安静创作。只有公开消息明确引用实际保存记录时才出现作品卡，
	// 后续未分享的第二版不能把之前的卡片悄悄替换成新版。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	tbox := &Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	result, err := tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"title": "安静写好的第一版", "format": "html", "content": "<button>第一版</button>"}))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ToolRunID string `json:"tool_run_id"`
	}
	if err = json.Unmarshal([]byte(result), &saved); err != nil {
		t.Fatal(err)
	}
	list, err := s.Artifacts(c.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("实际作品未保存: %+v %v", list, err)
	}
	quiet := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "暂时没有要分享的。", CreatedAt: now()}
	if err = s.Publish(quiet, c.Revision); err != nil {
		t.Fatal(err)
	}
	messages, err := s.Messages(c.ID, 0, 10)
	if err != nil || len(messages) != 1 || len(messages[0].Artifacts) != 0 {
		t.Fatalf("未引用的安静成果被自动公开: %+v %v", messages, err)
	}
	shared := Message{ID: newID("msg"), ConversationID: c.ID, AuthorID: m.ID, Body: "写好了第一版，放在这里。", Evidence: []string{saved.ToolRunID, saved.ToolRunID}, CreatedAt: now()}
	if err = s.Publish(shared, c.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"id": list[0].ID, "title": "尚未分享的第二版", "content": "<button>第二版</button>"})); err != nil {
		t.Fatal(err)
	}
	messages, err = s.Messages(c.ID, 0, 10)
	if err != nil || len(messages) != 2 || len(messages[0].Artifacts) != 0 || len(messages[1].Artifacts) != 1 {
		t.Fatalf("公开分享没有唯一作品卡: %+v %v", messages, err)
	}
	card := messages[1].Artifacts[0]
	if card.Version != 1 || card.Title != "安静写好的第一版" || card.Format != "html" || card.Content != "" {
		t.Fatalf("历史卡片泄露了未分享版本或冗长正文: %+v", card)
	}
	// 作者主动把同一版本分享到另一个群，应显示真实作品卡而不受创作地点限制。
	other, err := s.CreateConversation("另一个群", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	foreign := shared
	foreign.ID = newID("msg")
	foreign.ConversationID = other.ID
	if err = s.Publish(foreign, other.Revision); err != nil {
		t.Fatal(err)
	}
	messages, err = s.Messages(other.ID, 0, 10)
	if err != nil || len(messages) != 1 || len(messages[0].Artifacts) != 1 {
		t.Fatalf("作者跨群分享丢失作品卡: %+v %v", messages, err)
	}
}

func TestHTMLArtifactVersionsPreserveSourceAndFormat(t *testing.T) {
	// 场景：成员制作一个可点按的 HTML 小作品，之后修改时没有再次声明格式。
	// 版本应保持 HTML，原稿保留；工具返回只包含入口，避免整份源码重复进入上下文。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	tbox := &Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	// 旧成员只有网络工具时也具备基本的本地创作能力，无须再走偏好设置。
	m.Tools = []string{"search"}
	first := `<!doctype html><html lang="zh"><meta charset="utf-8"><button onclick="this.textContent='再走一步'">随便走走</button></html>`
	result, err := tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"title": "反攻略", "format": "html", "content": first}))
	if err != nil || !strings.Contains(result, `"status":"succeeded"`) || strings.Contains(result, "doctype") {
		t.Fatalf("保存未成功或返回了重复源码: %v %s", err, result)
	}
	list, err := s.Artifacts(c.ID)
	if err != nil || len(list) != 1 || list[0].Format != "html" || list[0].Content != first {
		t.Fatalf("HTML 源码未按原样保存: %+v %v", list, err)
	}
	second := strings.ReplaceAll(first, "再走一步", "现在回家也算完成")
	_, err = tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"id": list[0].ID, "title": "反攻略第二版", "content": second}))
	if err != nil {
		t.Fatal(err)
	}
	versions, err := s.ArtifactVersions(list[0].ID)
	if err != nil || len(versions) != 2 || versions[0].Format != "html" || versions[0].Content != second || versions[1].Content != first {
		t.Fatalf("编辑丢失格式或历史版本: %+v %v", versions, err)
	}
	// 不支持的格式不能落库，也不能伪装为已保存的第三版。
	result, err = tbox.Execute(context.Background(), c, m, w, toolCall("save_artifact", map[string]string{"id": list[0].ID, "title": "错误格式", "format": "exe", "content": "bytes"}))
	versions, _ = s.ArtifactVersions(list[0].ID)
	if err != nil || !strings.Contains(result, `"status":"failed"`) || len(versions) != 2 {
		t.Fatalf("非法格式仍产生版本: %v %s %+v", err, result, versions)
	}
}

func TestArtifactPreviewAndDownloadRespectVersionAndIsolation(t *testing.T) {
	// 场景：同一个成果先是 Markdown，后改为 HTML。旧版本仍只能按 Markdown 下载，
	// 新版本可在不透明来源中运行自己的脚本，但不能获得宿主应用的权限。
	s, c, m := fixture(t)
	w := addWork(t, s, c, m, "research")
	tbox := &Toolbox{Store: s, Config: testConfig(), Hub: NewHub()}
	_, err := tbox.saveArtifact(context.Background(), c, m, w, Artifact{Title: "第一版", Content: "# 原始笔记"})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := s.Artifacts(c.ID)
	id := list[0].ID
	html := `<!doctype html><button id="next">换一个</button><script>document.querySelector('button').onclick=()=>document.body.dataset.clicked='yes'</script>`
	if _, err = tbox.saveArtifact(context.Background(), c, m, w, Artifact{ID: id, Format: "html", Title: "第二版", Content: html}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(NewEngine(s, nil, testConfig()))
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://localhost"+path, nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder
	}
	preview := request("/api/artifacts/" + id + "/preview?version=2")
	if preview.Code != http.StatusOK || preview.Body.String() != html || preview.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("预览未返回指定 HTML 原稿: %d %s", preview.Code, preview.Body.String())
	}
	policy := preview.Header().Get("Content-Security-Policy")
	for _, required := range []string{"sandbox allow-scripts", "connect-src 'none'", "frame-src 'none'", "form-action 'none'", "frame-ancestors 'self'"} {
		if !strings.Contains(policy, required) {
			t.Fatalf("预览缺少权限边界 %q: %s", required, policy)
		}
	}
	for _, forbidden := range []string{"allow-same-origin", "allow-forms", "allow-popups", "allow-top-navigation"} {
		if strings.Contains(policy, forbidden) {
			t.Fatalf("预览获得了额外权限 %s", forbidden)
		}
	}
	if got := request("/api/artifacts/" + id + "/preview?version=1"); got.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("旧 Markdown 版本被当作可执行 HTML: %d", got.Code)
	}
	for _, path := range []string{"/download?version=1", "?download=1&version=1"} {
		got := request("/api/artifacts/" + id + path)
		if got.Code != http.StatusOK || got.Body.String() != "# 原始笔记" || !strings.Contains(got.Header().Get("Content-Disposition"), "-v1.md") {
			t.Fatalf("旧版下载入口未保留原稿: %d %s", got.Code, got.Body.String())
		}
	}
	got := request("/api/artifacts/" + id + "/download")
	if got.Body.String() != html || !strings.Contains(got.Header().Get("Content-Disposition"), "-v2.html") {
		t.Fatalf("默认下载没有取到最新 HTML: %s", got.Header())
	}
	for _, tc := range []struct {
		version string
		status  int
	}{{"-1", 400}, {"oops", 400}, {"99", 404}} {
		if got := request("/api/artifacts/" + id + "/preview?version=" + tc.version); got.Code != tc.status {
			t.Fatalf("无效版本 %s 返回 %d", tc.version, got.Code)
		}
	}
	// 沙箱脚本的来源为 null；即便将来放宽资源 CSP，它也不能借本机 API 修改聊天。
	r := httptest.NewRequest(http.MethodPost, "http://localhost/api/conversations/"+c.ID+"/messages", strings.NewReader(`{"body":"不得发送"}`))
	r.Header.Set("Origin", "null")
	r.Header.Set("Content-Type", "application/json")
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, r)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("不透明来源可以修改聊天: %d", denied.Code)
	}
}

func TestArtifactMigrationRetainsLegacyMarkdown(t *testing.T) {
	// 构造没有 format 列的旧数据库，再走正常启动。迁移只能加默认值，不能重写原稿。
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := Member{ID: "member_legacy", Name: "旧成员"}
	if err = s.AddMember(m); err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateConversation("旧成果", "group", []string{m.ID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Guard(c.ID, c.Revision, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO artifacts(id,version,conversation_id,member_id,title,content,source_id,created_at) VALUES('artifact_old',1,?,?,?,?,'',?)`, c.ID, m.ID, "旧笔记", "<b>历史文字</b>", now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`ALTER TABLE artifacts DROP COLUMN format`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	versions, err := s.ArtifactVersions("artifact_old")
	if err != nil || len(versions) != 1 || versions[0].Format != "markdown" || versions[0].Content != "<b>历史文字</b>" {
		t.Fatalf("迁移修改了原稿或把历史 HTML 当作新网页: %+v %v", versions, err)
	}
}
