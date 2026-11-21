package storage

import (
	"database/sql"
	"encoding/json"
	"time"

	"ircord/backend/internal/model"
)

type OutboxEntry struct {
	Command
	Payload     model.SendInput `json:"payload"`
	CreatedAt   time.Time       `json:"createdAt"`
	AttemptedAt time.Time       `json:"-"`
}

func (s *Store) OutboxEntry(owner, clientID string) (OutboxEntry, error) {
	entries, err := readOutbox(s.DB.Query(deliverySelect+" WHERE o.owner=? AND o.client_id=?", owner, clientID))
	if err != nil {
		return OutboxEntry{}, err
	}
	if len(entries) == 0 {
		return OutboxEntry{}, ErrNotFound
	}
	return entries[0], nil
}

func (s *Store) UnresolvedCommands(owner string, afterTime time.Time, afterID string, limit int) ([]OutboxEntry, error) {
	return readOutbox(s.DB.Query(deliverySelect+`
		WHERE o.owner=? AND o.state IN ('queued','pending','failed','uncertain','unconfirmed')
		AND (o.created_ns,o.client_id)>(?,?)
		ORDER BY o.created_ns,o.client_id LIMIT ?`, owner, afterTime.UnixNano(), afterID, limit))
}

func (s *Store) NetworkCommands(owner, network, state string, limit int) ([]OutboxEntry, error) {
	return readOutbox(s.DB.Query(deliverySelect+`
		JOIN entities e ON e.owner=o.owner AND e.kind='conversation' AND e.id=o.conversation
		WHERE o.owner=? AND o.state=? AND json_extract(e.payload,'$.networkId')=?
		ORDER BY o.created_ns,o.client_id LIMIT ?`, owner, state, network, limit))
}

func (s *Store) RecentUncertainCommands(owner, network string, limit int) ([]OutboxEntry, error) {
	return readOutbox(s.DB.Query(deliverySelect+`
		JOIN entities e ON e.owner=o.owner AND e.kind='conversation' AND e.id=o.conversation
		WHERE o.owner=? AND o.state IN ('uncertain','unconfirmed') AND json_extract(e.payload,'$.networkId')=?
		ORDER BY max(o.attempted_ns,o.created_ns) DESC,o.client_id LIMIT ?`, owner, network, limit))
}

const deliverySelect = `SELECT o.client_id,o.conversation,o.state,o.message_id,o.error,o.input,o.created_ns,o.attempted_ns,o.route_key FROM outbox o`

// Network removal clears sent history. Leave unresolved copies available to
// their owner and take queued sends out of automatic delivery.
func removeNetworkSends(tx *sql.Tx, owner, network string) error {
	const removeSent = "DELETE FROM outbox WHERE state='sent' AND "
	const stopSending = `UPDATE outbox SET state=CASE state WHEN 'queued' THEN 'failed' ELSE 'uncertain' END,
		error='The IRC network was removed.' WHERE state IN ('queued','pending') AND `
	for _, statement := range []string{removeSent, stopSending} {
		if _, err := tx.Exec(statement+openNetworkConversations, owner, owner, network); err != nil {
			return err
		}
		if _, err := tx.Exec(statement+closedNetworkConversations, owner, network, network, network, network); err != nil {
			return err
		}
	}
	return nil
}

func readOutbox(rows *sql.Rows, err error) ([]OutboxEntry, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEntry{}
	for rows.Next() {
		var entry OutboxEntry
		var message sql.NullString
		var input []byte
		var created, attempted int64
		if err := rows.Scan(&entry.ClientID, &entry.ConversationID, &entry.State, &message, &entry.Error, &input, &created, &attempted, &entry.Route); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(input, &entry.Payload); err != nil {
			return nil, err
		}
		entry.MessageID = message.String
		entry.CreatedAt = time.Unix(0, created).UTC()
		if attempted != 0 {
			entry.AttemptedAt = time.Unix(0, attempted).UTC()
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
