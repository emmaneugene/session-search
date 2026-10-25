package cursor

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Opening Cursor's database must not create files next to it. A WAL-mode
// database closed cleanly has no -wal or -shm file, and a plain mode=ro
// open creates both.
func TestOpenDBCreatesNoFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB); INSERT INTO cursorDiskKV VALUES('composerData:x', '{}')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Fatalf("setup left %s behind", suffix)
		}
	}

	ro, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.QueryRow(`SELECT count(*) FROM cursorDiskKV`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("read: n=%d err=%v", n, err)
	}
	ro.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("openDB created %s", suffix)
		}
	}
}
