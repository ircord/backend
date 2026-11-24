package filehost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// otherHost is a trusted file host whose addresses are not shaped like IRCord's:
// /uploads/<name>, like an IRC bouncer's. deletes says whether it answers
// OPTIONS with DELETE in Allow, and so whether IRCord may send it deletions.
type otherHost struct {
	*httptest.Server
	deleteRequests atomic.Int32
	deleteStatus   atomic.Int32
}

func newOtherHost(t *testing.T, advertisesDelete bool) *otherHost {
	t.Helper()
	h := &otherHost{}
	h.deleteStatus.Store(204)
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "OPTIONS":
			if advertisesDelete {
				w.Header().Set("Allow", "OPTIONS, POST, DELETE")
			}
			w.WriteHeader(204)
		case "POST":
			w.Header().Set("Location", fmt.Sprintf("/uploads/%d", time.Now().UnixNano()))
			w.WriteHeader(201)
		case "DELETE":
			h.deleteRequests.Add(1)
			w.WriteHeader(int(h.deleteStatus.Load()))
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *otherHost) hostname() string { u, _ := url.Parse(h.URL); return u.Hostname() }

func (h *otherHost) provider(service *Service, deletes bool) Provider {
	u, _ := url.Parse(h.URL)
	return service.upstream(&url.URL{Scheme: "https", Host: u.Host, Path: "/uploads"}, nil, deletes, "alice", "pw")
}

func hostService(t *testing.T, h *otherHost, options Options) (*Service, []byte) {
	t.Helper()
	options.TrustedHosts = []string{h.hostname()}
	options.Client = h.Client()
	service, data := quotaService(t, options)
	service.UseCredentials(func(string, string) (string, string, error) { return "alice", "pw", nil })
	return service, data
}

func say(t *testing.T, service *Service, text string) {
	t.Helper()
	if _, err := service.options.Store.DB.Exec("INSERT INTO messages(owner,conversation,id,upstream,created_ns,text,sender,kind,payload) VALUES('alice','c',?,'',1,?,'alice','message','{}')", fmt.Sprint(time.Now().UnixNano()), text); err != nil {
		t.Fatal(err)
	}
}

// A sent file is found in the messages whatever shape its host gives addresses, so the sweep
// never deletes one that a message shows, on a host that deletes or on one that does not.
func TestSweepKeepsAFileAMessageShowsOnAnyHostLayout(t *testing.T) {
	for _, advertisesDelete := range []bool{false, true} {
		t.Run(fmt.Sprintf("host deletes=%v", advertisesDelete), func(t *testing.T) {
			host := newOtherHost(t, advertisesDelete)
			service, data := hostService(t, host, Options{})
			up, err := service.Upload(context.Background(), "alice", "net", "sent.png", data, host.provider(service, advertisesDelete))
			if err != nil {
				t.Fatal(err)
			}
			say(t, service, "look "+up.URL)
			age(t, service, up.ID, 48*time.Hour)
			removed, err := service.SweepUnused(context.Background(), 24*time.Hour)
			if err != nil || removed != 0 || host.deleteRequests.Load() != 0 {
				t.Fatalf("a file a message shows was swept: removed %d, %d deletions, %v", removed, host.deleteRequests.Load(), err)
			}
			if _, err = service.Record("alice", up.ID); err != nil {
				t.Fatal("record dropped")
			}
		})
	}
}

// A host trusted only with the login, which never said it deletes files, is never sent a
// deletion, by the sweep, by the author or by account deletion.
func TestNoDeletionIsSentToAHostThatDidNotSayItDeletes(t *testing.T) {
	host := newOtherHost(t, false)
	service, data := hostService(t, host, Options{})
	ctx := context.Background()
	up, err := service.Upload(ctx, "alice", "net", "a.png", data, host.provider(service, false))
	if err != nil {
		t.Fatal(err)
	}
	if service.DeletableUpload("alice", up.URL) != "" {
		t.Fatal("offered to delete a file on a host that does not delete")
	}
	if err = service.Delete(ctx, "alice", up.ID); err == nil {
		t.Fatal("deleted a file on a host that does not delete")
	}
	age(t, service, up.ID, 48*time.Hour)
	if removed, err := service.SweepUnused(ctx, 24*time.Hour); err != nil || removed != 0 {
		t.Fatalf("sweep: %d %v", removed, err)
	}
	if host.deleteRequests.Load() != 0 {
		t.Fatalf("%d deletions sent", host.deleteRequests.Load())
	}
}

// A file stored before the host's answer was recorded has no flag, so it is not deleted either.
func TestFilesWithoutARecordedDeleteFlagAreNotDeleted(t *testing.T) {
	host := newOtherHost(t, true)
	service, data := hostService(t, host, Options{})
	up, err := service.Upload(context.Background(), "alice", "net", "a.png", data, host.provider(service, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.options.Store.DB.Exec("DELETE FROM file_remote WHERE id=?", up.ID); err != nil {
		t.Fatal(err)
	}
	if service.deletableUpstream(up.ID, up.URL) {
		t.Fatal("deletable without a recorded answer")
	}
}

// Records the sweep may not act on must not fill its batch and starve the rest.
func TestSweepBatchIsNotFilledByFilesItMayNotDelete(t *testing.T) {
	host := newOtherHost(t, false)
	service, data := hostService(t, host, Options{})
	db := service.options.Store.DB
	old := time.Now().Add(-72 * time.Hour).UnixNano()
	for i := 0; i < SweepBatch+5; i++ {
		id := fmt.Sprintf("up%03d", i)
		if _, err := db.Exec("INSERT INTO files(id,owner,name,mime,size,sha256,backend,object_key,token_hash,created_ns) VALUES(?,?,?,?,?,?,?,?,?,?)", id, "alice", "x.png", "image/png", 1, "", "upstream", "", []byte{1}, old); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO file_metadata(id,network,payload) VALUES(?,?,?)", id, "net", fmt.Sprintf(`{"url":"https://untrusted.example/uploads/%d"}`, i)); err != nil {
			t.Fatal(err)
		}
		// A host that said it deletes, but is no longer trusted.
		if _, err := db.Exec("INSERT INTO file_remote(id,can_delete) VALUES(?,1)", id); err != nil {
			t.Fatal(err)
		}
	}
	local, err := service.Upload(context.Background(), "alice", "", "local.png", data, service.LocalProvider())
	if err != nil {
		t.Fatal(err)
	}
	age(t, service, local.ID, 48*time.Hour)
	for round := 0; round < 3; round++ {
		if _, err = service.SweepUnused(context.Background(), 24*time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = service.Record("alice", local.ID); err == nil {
		t.Fatal("an unused local upload survived three sweeps behind records the sweep may not delete")
	}
	var kept int
	if err = db.QueryRow("SELECT count(*) FROM files WHERE backend='upstream'").Scan(&kept); err != nil || kept != SweepBatch+5 {
		t.Fatalf("files on a host that is no longer trusted were removed: %d %v", kept, err)
	}
}

// Deleting an account is retried when the host fails; the retry has the same logins and
// finishes the job, instead of forgetting the files it could not delete.
func TestAccountDeletionRetriesAFailedHostDeletion(t *testing.T) {
	host := newOtherHost(t, true)
	service, data := hostService(t, host, Options{})
	ctx := context.Background()
	up, err := service.Upload(ctx, "alice", "net", "a.png", data, host.provider(service, true))
	if err != nil {
		t.Fatal(err)
	}
	logins := func(string, string) (string, string, error) { return "alice", "pw", nil }
	host.deleteStatus.Store(503)
	if err = service.DeleteAccountUsing(ctx, "alice", logins); err == nil {
		t.Fatal("a host that failed did not fail the deletion")
	}
	if _, err = service.Record("alice", up.ID); err != nil {
		t.Fatal("the record of a file the host did not delete was dropped")
	}
	host.deleteStatus.Store(204)
	if err = service.DeleteAccountUsing(ctx, "alice", logins); err != nil {
		t.Fatal(err)
	}
	if host.deleteRequests.Load() != 2 || files(t, service, "alice") != 0 {
		t.Fatalf("%d deletion requests, %d files left", host.deleteRequests.Load(), files(t, service, "alice"))
	}
}

// A fault of this server's storage, and another account's file, are told apart from a missing file.
func TestDeleteReceivedTellsFaultsFromMissingFiles(t *testing.T) {
	store, dir, data := fixture(t)
	blob := Local{Root: dir + "/files"}
	service, err := New(Options{Store: store, Blob: blob, PublicURL: "https://chat.example"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	up, err := service.Receive(ctx, "alice", "a.png", data)
	if err != nil {
		t.Fatal(err)
	}
	token := up.URL[len("https://chat.example/files/")+len(up.ID)+1:]
	token = token[:len(token)-len("/a.png")]
	if err = service.DeleteReceived(ctx, "alice", up.ID, "wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong token: %v", err)
	}
	if err = service.DeleteReceived(ctx, "alice", "missing", token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if err = service.DeleteReceived(ctx, "bob", up.ID, token); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another account: %v", err)
	}
	// Storage that fails to delete is a fault, not a missing file.
	broken := failingBlob{Local: blob, key: up.ID}
	service.options.Blob = broken
	if err = service.DeleteReceived(ctx, "alice", up.ID, token); err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrForbidden) {
		t.Fatalf("storage fault: %v", err)
	}
	if _, err = service.Record("alice", up.ID); err != nil {
		t.Fatal("record dropped though the file was not deleted")
	}
	service.options.Blob = blob
	if err = service.DeleteReceived(ctx, "alice", up.ID, token); err != nil {
		t.Fatal(err)
	}
}
