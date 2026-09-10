package daemon

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
	"time"
)

type Store struct{ db *sql.DB }
type Watch struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}
type Receipt struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Status  string          `json:"status"`
	Result  json.RawMessage `json:"result"`
	Created string          `json:"created"`
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func openStore(dir string) (*Store, string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, "access-token")
	token, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		token = []byte(randomID())
		err = os.WriteFile(path, token, 0600)
	}
	if err != nil {
		return nil, "", err
	}
	if len(token) < 32 {
		return nil, "", errors.New("access token is too short")
	}
	dbPath := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, "", err
	}
	_ = f.Close()
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, "", err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS watches(name TEXT PRIMARY KEY,generation TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS receipts(id TEXT PRIMARY KEY,name TEXT NOT NULL,status TEXT NOT NULL,result TEXT NOT NULL,created TEXT NOT NULL);
 UPDATE receipts SET status='unknown' WHERE status='submitting';`)
	if err != nil {
		db.Close()
		return nil, "", err
	}
	return &Store{db}, string(token), nil
}
func (s *Store) watches() ([]Watch, error) {
	rows, err := s.db.Query("SELECT name,generation FROM watches ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Watch{}
	for rows.Next() {
		var w Watch
		if err = rows.Scan(&w.Name, &w.Generation); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (s *Store) watch(w Watch, enabled bool) error {
	var err error
	if enabled {
		_, err = s.db.Exec("INSERT INTO watches VALUES(?,?) ON CONFLICT(name) DO UPDATE SET generation=excluded.generation", w.Name, w.Generation)
	} else {
		_, err = s.db.Exec("DELETE FROM watches WHERE name=?", w.Name)
	}
	return err
}
func (s *Store) receipt(id string) (*Receipt, error) {
	var r Receipt
	var result string
	err := s.db.QueryRow("SELECT id,name,status,result,created FROM receipts WHERE id=?", id).Scan(&r.ID, &r.Name, &r.Status, &result, &r.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	r.Result = json.RawMessage(result)
	return &r, err
}
func (s *Store) begin(id, name string) error {
	_, err := s.db.Exec("INSERT INTO receipts VALUES(?,?,'submitting','{}',?)", id, name, time.Now().UTC().Format(time.RFC3339))
	return err
}
func (s *Store) finish(id, status string, result json.RawMessage) error {
	_, err := s.db.Exec("UPDATE receipts SET status=?,result=? WHERE id=?", status, string(result), id)
	return err
}
func (s *Store) receipts() ([]Receipt, error) {
	rows, err := s.db.Query("SELECT id,name,status,result,created FROM receipts ORDER BY created DESC LIMIT 50")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Receipt{}
	for rows.Next() {
		var r Receipt
		var result string
		if err = rows.Scan(&r.ID, &r.Name, &r.Status, &result, &r.Created); err != nil {
			return nil, err
		}
		r.Result = json.RawMessage(result)
		out = append(out, r)
	}
	return out, rows.Err()
}
