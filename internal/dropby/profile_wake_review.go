package dropby

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// 人物文本和待判断事项同时提交。连续编辑合并为一次判断，仍以尚未判断
// 的第一版为起点，避免丢掉上一次编辑里来不及处理的作息变化。
func queueProfileWakeReview(tx *sql.Tx, m Member) error {
	_, err := tx.Exec(`INSERT INTO profile_wake_reviews(member_id,before_revision,revision,due_at) VALUES(?,?,?,?)
ON CONFLICT(member_id) DO UPDATE SET
 before_revision=CASE WHEN profile_wake_reviews.revision=excluded.before_revision
                      THEN profile_wake_reviews.before_revision ELSE excluded.before_revision END,
 revision=excluded.revision, due_at=excluded.due_at`,
		m.ID, m.ProfileRevision-1, m.ProfileRevision, time.Now().UnixMilli())
	return err
}

type profileWakeReview struct {
	member   string
	revision int
}

func (e *Engine) dispatchProfileWakeReviews(ctx context.Context) {
	// 读完队列再启动工作，避免在模型调用期间占用 SQLite 的连接。
	var pending []profileWakeReview
	err := func() error {
		rows, err := e.Store.db.QueryContext(ctx, `SELECT member_id,revision FROM profile_wake_reviews WHERE due_at<=?`, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var review profileWakeReview
			if err := rows.Scan(&review.member, &review.revision); err != nil {
				return err
			}
			pending = append(pending, review)
		}
		return rows.Err()
	}()
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("读取待判断的人物作息失败", "error", err)
		}
		return
	}
	for _, review := range pending {
		if _, running := e.wakeReviews.LoadOrStore(review.member, true); running {
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer e.wakeReviews.Delete(review.member)
			if err := e.reviewProfileWake(ctx, review); err != nil && ctx.Err() == nil {
				slog.Warn("后台判断人物作息失败", "member", review.member, "revision", review.revision, "error", err)
			}
		}()
	}
}

func (e *Engine) reviewProfileWake(ctx context.Context, review profileWakeReview) error {
	s := e.Store
	clear := func() error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM profile_wake_reviews WHERE member_id=? AND revision=?`, review.member, review.revision)
		return err
	}
	after, err := s.Member(review.member)
	if err != nil {
		return err
	}
	if after.ProfileRevision != review.revision {
		return clear()
	}
	before := after
	var domains string
	// 轮询读到的待办可能已被上一轮处理完，或在启动前就被新一版替换。
	err = s.db.QueryRowContext(ctx, `SELECT p.profile,p.domain_profiles
FROM profile_wake_reviews q JOIN profile_versions p ON p.member_id=q.member_id AND p.revision=q.before_revision
WHERE q.member_id=? AND q.revision=? AND q.due_at<=?`, review.member, review.revision, time.Now().UnixMilli()).Scan(&before.Profile, &domains)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(domains), &before.DomainProfiles); err != nil {
		return err
	}
	rhythm, err := s.WakeRhythm(review.member)
	if err != nil {
		return err
	}
	patch, adjust, err := judgeWakeRhythm(ctx, e.Model, before, after, "用户直接编辑了人物设定", rhythm)
	if ctx.Err() != nil {
		return ctx.Err() // 服务重启后仍可处理这条已保存的判断任务。
	}
	if errors.Is(err, ErrBudget) || recoverableNetwork(err) {
		// 等预算或网络恢复。新编辑会刷新待判断版本和到期时间。
		due := time.Now().Add(e.Config.ExploreInterval)
		if errors.Is(err, ErrBudget) {
			due = nextBudgetWeek()
		}
		_, retryErr := s.db.ExecContext(ctx, `UPDATE profile_wake_reviews SET due_at=? WHERE member_id=? AND revision=?`, due.UnixMilli(), review.member, review.revision)
		return errors.Join(err, retryErr)
	}
	if err != nil || !adjust {
		return errors.Join(err, clear())
	}
	// 先验证模型给出的作息，出错时只结束这次判断，已保存的人格仍生效。
	if _, err := applyWakePatch(rhythm, patch); err != nil {
		return errors.Join(err, clear())
	}
	return e.applyProfileWakeReview(ctx, review, rhythm, patch)
}

func (e *Engine) applyProfileWakeReview(ctx context.Context, review profileWakeReview, previous wakeRhythm, patch wakePatch) error {
	tx, err := e.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 在写入事务里核对版本，防止检查之后又保存了一次。作息也可能
	// 由成员主动修改，只有仍和判断时一致才应用这份结果。
	var revision int
	if err := tx.QueryRow(`SELECT coalesce(json_extract(data,'$.profile_revision'),0) FROM members WHERE id=?`, review.member).Scan(&revision); err != nil {
		return err
	}
	if revision == review.revision {
		current, err := loadWakeTx(tx, review.member)
		if err != nil {
			return err
		}
		if sameRhythm(previous, current) {
			if err := applyWakeDecision(tx, review.member, patch); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`DELETE FROM profile_wake_reviews WHERE member_id=? AND revision=?`, review.member, review.revision); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.Hub.Publish("")
	return nil
}
