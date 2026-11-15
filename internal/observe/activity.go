package observe

import (
	"encoding/json"
)

// ActivityEvent is the small subset of execution evidence the chat can use.
// The activity panel never reads request snapshots or model context.
type ActivityEvent struct {
	WorkID    string `json:"-"`
	MemberID  string `json:"member_id,omitempty"`
	Kind      string `json:"kind"`
	Step      string `json:"step"`
	MessageID string `json:"-"`
	RoomID    string `json:"-"`
	At        string `json:"at"`
	Output    string `json:"output,omitempty"`
}

// ActivityDelivery links a legacy message to its work by the recorded delivery
// ID. The chat store still verifies its destination and author.
type ActivityDelivery struct {
	WorkID    string `json:"work_id"`
	MemberID  string `json:"member_id"`
	MessageID string `json:"message_id"`
}

func (l *Log) ActivityDeliveries(room string) ([]ActivityDelivery, error) {
	if l == nil {
		return nil, nil
	}
	rows, err := l.db.Query(`
SELECT DISTINCT r.work_id,coalesce(nullif(s.member_id,''),r.member_id),s.business_id
FROM steps s JOIN runs r ON r.id=s.run_id
WHERE s.target_conversation_id=? AND s.business_id<>''
 AND s.kind='message' AND s.name='send_message' AND s.status IN ('sent','reused')`, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []ActivityDelivery
	for rows.Next() {
		var d ActivityDelivery
		if err := rows.Scan(&d.WorkID, &d.MemberID, &d.MessageID); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

// ActivityEvents supplies input-count failures that occurred before any tool
// call could be recorded.
func (l *Log) ActivityEvents(workIDs []string, detail bool) ([]ActivityEvent, error) {
	if l == nil || len(workIDs) == 0 {
		return nil, nil
	}
	keys, err := json.Marshal(workIDs)
	if err != nil {
		return nil, err
	}
	rows, err := l.db.Query(`
SELECT r.work_id,coalesce(nullif(s.member_id,''),r.member_id),s.kind,s.name,s.business_id,s.target_conversation_id,s.started_at,
       CASE WHEN ? AND s.name='input_tokens' THEN coalesce(p.output,'') ELSE '' END
FROM runs r JOIN steps s ON s.run_id=r.id LEFT JOIN snapshots p ON ? AND p.step_id=s.id
WHERE r.work_id IN (SELECT value FROM json_each(?))
  AND s.name='input_tokens' AND s.status='failed'
ORDER BY s.seq`, detail, detail, string(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []ActivityEvent
	for rows.Next() {
		var e ActivityEvent
		if err := rows.Scan(&e.WorkID, &e.MemberID, &e.Kind, &e.Step, &e.MessageID, &e.RoomID, &e.At, &e.Output); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}
