package dropby

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// SQL 可以自行投影、关联和聚合。列名与行分开保存，重复列名、NULL 和字段顺序都能回读。
type recallTable struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

func (s *Store) queryRecallSQL(ctx context.Context, member, query string) (recallTable, error) {
	var table recallTable
	// 每次查询独占一个内存连接。在同一读事务里从业务库取出此刻可见的记录和分享；
	// 在执行成员的 SQL 前解除连接。即使成员自定义子查询或同名 CTE，也只可能读到自己的副本。
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return table, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return table, err
	}
	defer conn.Close()
	path, err := filepath.Abs(s.path)
	if err != nil {
		return table, err
	}
	source := &url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS source", source.String()); err != nil {
		return table, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return table, err
	}
	defer tx.Rollback()

	var selects []string
	for _, src := range recallSources {
		selects = append(selects, "SELECT '"+src.kind+"' AS kind,* FROM ("+src.sql+")")
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE records AS WITH scoped(kind,id,num,author_id,conversation_id,created_at,title,body,seq,save_run_id) AS (`+strings.Join(selects, " UNION ALL ")+`)
	SELECT kind,id, CASE WHEN kind='artifact' THEN num END version, CASE WHEN kind='memory' THEN num END revision,
	seq,title,body,author_id,author_id=?1 mine,conversation_id,created_at,datetime(created_at,'localtime') local_time,save_run_id FROM scoped`, member, "")
	if err != nil {
		return table, err
	}
	_, err = tx.ExecContext(ctx, `CREATE TABLE shares AS `+recallShareCTE+`SELECT s.message_id,s.conversation_id,c.name conversation_name,s.aid artifact_id,s.ver version,s.save_run_id
	FROM visible_shares s JOIN conversations c ON c.id=s.conversation_id`, member, "")
	if err != nil {
		return table, err
	}
	if err = tx.Commit(); err != nil {
		return table, err
	}
	if _, err = conn.ExecContext(ctx, "DETACH DATABASE source"); err != nil {
		return table, err
	}

	// SQLite 自己检查语句要执行的操作，允许查询、递归和普通表达式。成员的 SQL
	// 不能写回、连接文件或加载扩展。权限绑定此物理连接，连接关闭后副本随之释放。
	err = conn.Raw(func(driverConn any) error {
		return driverConn.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(recallSQLAccess)
	})
	if err != nil {
		return table, err
	}
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return table, fmt.Errorf("recall SQL 查询失败：%w", err)
	}
	defer rows.Close()
	if table.Columns, err = rows.Columns(); err != nil {
		return table, err
	}
	table.Rows = [][]any{}
	for rows.Next() {
		values := make([]any, len(table.Columns))
		ptrs := make([]any, len(values))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return table, err
		}
		// JSON 不能编码 SQLite 的无穷浮点值。保留其可读表示，其余值保留原类型。
		for i, v := range values {
			if f, ok := v.(float64); ok && math.IsInf(f, 0) {
				values[i] = fmt.Sprint(f)
			}
		}
		table.Rows = append(table.Rows, values)
	}
	return table, rows.Err()
}

func recallSQLAccess(action sqlite.AuthorizerActionCode, table, function, schema, _ string) sqlite.AuthorizerReturnCode {
	switch action {
	case sqlite.AuthSelect, sqlite.AuthRecursive:
		return sqlite.AuthorizerOK
	case sqlite.AuthRead:
		if (schema == "main" || schema == "") && (table == "records" || table == "shares") {
			return sqlite.AuthorizerOK
		}
	case sqlite.AuthFunction:
		if !strings.EqualFold(function, "load_extension") {
			return sqlite.AuthorizerOK
		}
	}
	return sqlite.AuthorizerDeny
}
