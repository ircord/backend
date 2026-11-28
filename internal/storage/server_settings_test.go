package storage

import (
	"context"
	"testing"
)

// A server setting survives reopening the database, and saving it again
// replaces the value and who changed it.
func TestServerSettingPersists(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if v, err := s.ServerSetting(ctx, SettingEncryption); err != nil || v.Value != "" || !v.UpdatedAt.IsZero() {
		t.Fatalf("unsaved setting = %+v, %v", v, err)
	}
	if err = s.SaveServerSetting(ctx, SettingEncryption, "experimental", "alice"); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveServerSetting(ctx, SettingEncryption, "off", "bob"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := s.ServerSetting(ctx, SettingEncryption)
	if err != nil || v.Value != "off" || v.UpdatedBy != "bob" || v.UpdatedAt.IsZero() {
		t.Fatalf("saved setting = %+v, %v", v, err)
	}
}
