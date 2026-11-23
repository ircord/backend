package storage

import (
	"testing"
	"time"

	"ircord/backend/internal/model"
)

type catalogRow struct {
	url, kind, identity string
	previewed           bool
}

func catalogRowsOf(t *testing.T, s *Store, message string) []catalogRow {
	t.Helper()
	rows, err := s.DB.Query("SELECT url,kind,identity,previewed FROM message_links WHERE owner='alice' AND message=? ORDER BY idx", message)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []catalogRow{}
	for rows.Next() {
		var r catalogRow
		if err = rows.Scan(&r.url, &r.kind, &r.identity, &r.previewed); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func catalogStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	account(t, s, "alice")
	if _, err = s.Commit("alice", "conversation.updated", model.Conversation{ID: "net:#pics", NetworkID: "net", Kind: "channel"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLinkCatalogIndexesTextWithoutGalleryExtraction(t *testing.T) {
	s := catalogStore(t)
	msg := model.Message{ID: "net:1", ConversationID: "net:#pics", SenderID: "carol", Kind: "message", CreatedAt: time.Now().UTC(),
		Text: "http://plain.test/a.png then https://i.imgur.com/a.png and https://news.test/story?id=1#top",
		GIF:  nil}
	if _, err := s.Commit("alice", "message.created", msg); err != nil {
		t.Fatal(err)
	}
	got := catalogRowsOf(t, s, "net:1")
	want := []catalogRow{
		{"http://plain.test/a.png", "image", "http://plain.test/a.png", false},
		{"https://i.imgur.com/a.png", "image", "https://i.imgur.com/a.png", false},
		{"https://news.test/story?id=1#top", "link", "https://news.test/story?id=1", false},
	}
	if len(got) != len(want) {
		t.Fatalf("rows: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	groups, _, err := s.Links("alice", LinkFilter{}, "", 10, 5)
	if err != nil || len(groups) != 3 {
		t.Fatalf("catalog: %+v %v", groups, err)
	}

	// Uploads and picked GIFs do not generate extra catalog entries.
	upload := model.Message{ID: "net:2", ConversationID: "net:#pics", SenderID: "carol", Kind: "message", CreatedAt: time.Now().UTC(),
		Text: "https://chat.example/files/abc/x/cat.png", Attachment: &model.Attachment{URL: "https://chat.example/files/abc/x/cat.png", Name: "cat.png", ContentType: "image/png"}}
	if _, err = s.Commit("alice", "message.created", upload); err != nil {
		t.Fatal(err)
	}
	if rows := catalogRowsOf(t, s, "net:2"); len(rows) != 1 || rows[0].identity == "" {
		t.Fatalf("upload rows: %+v", rows)
	}
	upload.ID, upload.Text = "net:3", "a photo"
	if _, err = s.Commit("alice", "message.created", upload); err != nil {
		t.Fatal(err)
	}
	if rows := catalogRowsOf(t, s, "net:3"); len(rows) != 0 {
		t.Fatalf("indexed attachment: %+v", rows)
	}
}

// Rows indexed under the gallery-only rules (version 1) gain their catalog
// identity when the backfill runs again under the current version.
func TestBackfillAddsCatalogIdentitiesToOlderRows(t *testing.T) {
	s := catalogStore(t)
	msg := model.Message{ID: "net:1", ConversationID: "net:#pics", SenderID: "carol", Kind: "message", Text: "https://i.imgur.com/a.png https://news.test/", CreatedAt: time.Now().UTC()}
	if err := s.SaveHistory("alice", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DELETE FROM message_links WHERE identity!='' AND kind='link'; UPDATE message_links SET identity='',host='',previewed=0; INSERT INTO index_jobs(name,version,last_rowid,done,updated_ns) VALUES('message-links',1,1000000,1,0) ON CONFLICT(name) DO UPDATE SET version=1,last_rowid=1000000,done=1"); err != nil {
		t.Fatal(err)
	}
	if groups, _, _ := s.Links("alice", LinkFilter{}, "", 10, 5); len(groups) != 0 {
		t.Fatalf("catalog before re-index: %+v", groups)
	}
	for {
		done, err := s.BackfillLinksStep()
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if groups, _, _ := s.Links("alice", LinkFilter{}, "", 10, 5); len(groups) != 2 {
		t.Fatalf("catalog after re-index: %+v", groups)
	}
}

// A message stored with a masked link under the earlier rules keeps a wrong
// url until the backfill runs again; rebuilding is idempotent.
func TestBackfillRebuildsMaskedLinkRows(t *testing.T) {
	s := catalogStore(t)
	msg := model.Message{ID: "net:1", ConversationID: "net:#pics", SenderID: "carol", Kind: "message", Text: "see [https://a.test/shown](https://b.test/real)", CreatedAt: time.Now().UTC()}
	if err := s.SaveHistory("alice", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE message_links SET url='https://a.test/shown](https://b.test/real)',identity='https://a.test/shown](https://b.test/real)',host='a.test'; INSERT INTO index_jobs(name,version,last_rowid,done,updated_ns) VALUES('message-links',4,1000000,1,0) ON CONFLICT(name) DO UPDATE SET version=4,last_rowid=1000000,done=1"); err != nil {
		t.Fatal(err)
	}
	if done, err := s.LinkIndexComplete(); done || err != nil {
		t.Fatalf("stale index reported complete: %v %v", done, err)
	}
	for round := 0; round < 2; round++ {
		if _, err := s.DB.Exec("UPDATE index_jobs SET version=4,last_rowid=0,done=0 WHERE ?=1", round); err != nil {
			t.Fatal(err)
		}
		for {
			done, err := s.BackfillLinksStep()
			if err != nil {
				t.Fatal(err)
			}
			if done {
				break
			}
		}
		rows := catalogRowsOf(t, s, "net:1")
		if len(rows) != 1 || rows[0].url != "https://b.test/real" || rows[0].identity != "https://b.test/real" {
			t.Fatalf("round %d rows: %+v", round, rows)
		}
	}
}
