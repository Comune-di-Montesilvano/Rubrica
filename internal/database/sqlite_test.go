package database

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := InitDB(path)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUpsertContactPersistsArea(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{
		UID:         "mario.rossi",
		DisplayName: "Mario Rossi",
		Area:        "interni",
		LastSync:    time.Now(),
	}
	if err := db.UpsertContact(c); err != nil {
		t.Fatalf("UpsertContact failed: %v", err)
	}

	got, err := db.GetContact("mario.rossi")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got == nil {
		t.Fatal("GetContact returned nil")
	}
	if got.Area != "interni" {
		t.Errorf("Area = %q, want %q", got.Area, "interni")
	}
}

func TestUpsertContactRespectsManualOverrideForArea(t *testing.T) {
	db := newTestDB(t)
	c := &Contact{UID: "u1", DisplayName: "U1", Area: "interni", LastSync: time.Now()}
	if err := db.UpsertContact(c); err != nil {
		t.Fatalf("initial UpsertContact failed: %v", err)
	}
	if err := db.UpdateContactOverride("u1", "u1@example.com", "12345"); err != nil {
		t.Fatalf("UpdateContactOverride failed: %v", err)
	}

	c2 := &Contact{UID: "u1", DisplayName: "U1", Area: "esterni", LastSync: time.Now()}
	if err := db.UpsertContact(c2); err != nil {
		t.Fatalf("second UpsertContact failed: %v", err)
	}

	got, err := db.GetContact("u1")
	if err != nil {
		t.Fatalf("GetContact failed: %v", err)
	}
	if got.Area != "interni" {
		t.Errorf("Area = %q after override, want unchanged %q", got.Area, "interni")
	}
}

func TestCountByArea(t *testing.T) {
	db := newTestDB(t)
	contacts := []*Contact{
		{UID: "a", DisplayName: "A", Area: "interni", LastSync: time.Now()},
		{UID: "b", DisplayName: "B", Area: "interni", LastSync: time.Now()},
		{UID: "c", DisplayName: "C", Area: "esterni", LastSync: time.Now()},
		{UID: "d", DisplayName: "D", Area: "politica", LastSync: time.Now()},
	}
	for _, c := range contacts {
		if err := db.UpsertContact(c); err != nil {
			t.Fatalf("UpsertContact(%s) failed: %v", c.UID, err)
		}
	}

	counts, err := db.CountByArea()
	if err != nil {
		t.Fatalf("CountByArea failed: %v", err)
	}
	want := map[string]int{"interni": 2, "esterni": 1, "politica": 1}
	for area, wantCount := range want {
		if counts[area] != wantCount {
			t.Errorf("counts[%q] = %d, want %d", area, counts[area], wantCount)
		}
	}
}
