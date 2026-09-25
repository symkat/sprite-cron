package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"time"
)

type Store struct {
	DB   *sql.DB
	Path string
}

func Open(path string, create bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = abs
	if !create {
		if _, err = os.Stat(path); err != nil {
			return nil, fmt.Errorf("database not found: initialize or start serve first: %w", err)
		}
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if create {
		f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if e != nil {
			return nil, e
		}
		f.Close()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Path: path}
	if _, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;`); err != nil {
		db.Close()
		return nil, err
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 1 {
		db.Close()
		return nil, errors.New("database schema is newer than this binary")
	}
	if version == 0 {
		if !create {
			db.Close()
			return nil, errors.New("database has not been initialized")
		}
		_, err = db.Exec(schema)
		if err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

const schema = `BEGIN IMMEDIATE;
CREATE TABLE users(id TEXT PRIMARY KEY,username TEXT UNIQUE NOT NULL,password TEXT NOT NULL,role TEXT NOT NULL,disabled INTEGER NOT NULL DEFAULT 0,service INTEGER NOT NULL DEFAULT 0);
CREATE TABLE sessions(digest TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),csrf TEXT NOT NULL,created INTEGER NOT NULL,last_seen INTEGER NOT NULL,expires INTEGER NOT NULL);
CREATE TABLE api_tokens(id TEXT PRIMARY KEY,digest TEXT NOT NULL UNIQUE,user_id TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,scopes TEXT NOT NULL,targets TEXT NOT NULL,expires INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0,last_used INTEGER NOT NULL DEFAULT 0);
CREATE TABLE credentials(id TEXT PRIMARY KEY,label TEXT NOT NULL,kind TEXT NOT NULL,encrypted TEXT NOT NULL,version INTEGER NOT NULL DEFAULT 1);
CREATE TABLE targets(id TEXT PRIMARY KEY,data TEXT NOT NULL);
CREATE TABLE jobs(id TEXT PRIMARY KEY,target_id TEXT NOT NULL REFERENCES targets(id),enabled INTEGER NOT NULL,next_run INTEGER NOT NULL,revision INTEGER NOT NULL,data TEXT NOT NULL);
CREATE INDEX due_jobs ON jobs(enabled,next_run);
CREATE TABLE runs(id TEXT PRIMARY KEY,job_id TEXT NOT NULL,target_id TEXT NOT NULL,revision INTEGER NOT NULL,scheduled INTEGER NOT NULL,created INTEGER NOT NULL,status TEXT NOT NULL,start_before INTEGER NOT NULL,deadline INTEGER NOT NULL DEFAULT 0,attempt INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL DEFAULT 0,session_id TEXT NOT NULL DEFAULT '',cancel INTEGER NOT NULL DEFAULT 0,resolved INTEGER NOT NULL DEFAULT 0,manual INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',snapshot TEXT NOT NULL,stdout TEXT NOT NULL DEFAULT '',stderr TEXT NOT NULL DEFAULT '',truncated INTEGER NOT NULL DEFAULT 0,exit_code INTEGER,http_status INTEGER NOT NULL DEFAULT 0,request_key TEXT NOT NULL DEFAULT '');
CREATE UNIQUE INDEX occurrence ON runs(job_id,revision,scheduled) WHERE manual=0;
CREATE UNIQUE INDEX manual_key ON runs(job_id,request_key) WHERE request_key<>'';
CREATE INDEX pending_runs ON runs(status,next_attempt,created);
CREATE INDEX job_runs ON runs(job_id,created DESC);
CREATE TABLE attempts(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,number INTEGER NOT NULL,started INTEGER NOT NULL,finished INTEGER,status TEXT NOT NULL,reason TEXT NOT NULL DEFAULT '',session_id TEXT NOT NULL DEFAULT '',stdout TEXT NOT NULL DEFAULT '',stderr TEXT NOT NULL DEFAULT '',truncated INTEGER NOT NULL DEFAULT 0,exit_code INTEGER,http_status INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(run_id,number));
CREATE TABLE audit(id INTEGER PRIMARY KEY,at INTEGER NOT NULL,actor TEXT NOT NULL,action TEXT NOT NULL,object TEXT NOT NULL,detail TEXT NOT NULL);
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
PRAGMA user_version=1; COMMIT;`

type querier interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func audit(q querier, actor, action, object, detail string) error {
	_, err := q.Exec(`INSERT INTO audit(at,actor,action,object,detail) VALUES(?,?,?,?,?)`, nowMS(), actor, action, object, detail)
	return err
}
func transaction(db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func targetFrom(q querier, id string) (Target, error) {
	var t Target
	var data string
	err := q.QueryRow(`SELECT data FROM targets WHERE id=?`, id).Scan(&data)
	if err == nil {
		err = json.Unmarshal([]byte(data), &t)
	}
	return t, err
}
func jobFrom(q querier, id string) (Job, error) {
	var j Job
	var data string
	err := q.QueryRow(`SELECT data FROM jobs WHERE id=?`, id).Scan(&data)
	if err == nil {
		err = json.Unmarshal([]byte(data), &j)
	}
	return j, err
}
func (s *Store) Jobs() ([]Job, error) {
	rows, err := s.DB.Query(`SELECT data FROM jobs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var j Job
		if err = json.Unmarshal([]byte(data), &j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func (s *Store) Targets() ([]Target, error) {
	rows, err := s.DB.Query(`SELECT data FROM targets ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Target{}
	for rows.Next() {
		var data string
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var t Target
		if err = json.Unmarshal([]byte(data), &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) SaveJob(j Job, create bool, actor string) error {
	if err := j.Validate(); err != nil {
		return err
	}
	sc, _ := schedule(j.Schedule, j.Timezone)
	return transaction(s.DB, func(tx *sql.Tx) error {
		if _, err := targetFrom(tx, j.TargetID); err != nil {
			return fmt.Errorf("target does not exist")
		}
		if create {
			var count int
			tx.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count)
			if count >= 1000 {
				return errors.New("job limit reached (1000)")
			}
			j.ID = randomID()
			j.Revision = 1
		} else {
			old, err := jobFrom(tx, j.ID)
			if err != nil {
				return err
			}
			if old.Revision != j.Revision {
				return ErrConflict
			}
			j.Revision++
			if _, err = tx.Exec(`UPDATE runs SET status=CASE WHEN status='retry_wait' AND EXISTS(SELECT 1 FROM attempts WHERE run_id=runs.id AND number=runs.attempt AND status='unknown') THEN 'unknown' ELSE 'cancelled' END,reason='job edited; queued execution cancelled' WHERE job_id=? AND manual=0 AND status IN ('queued','retry_wait')`, j.ID); err != nil {
				return err
			}
		}
		j.NextRun = sc.Next(time.Now()).UnixMilli()
		if _, err := tx.Exec(`INSERT INTO jobs(id,target_id,enabled,next_run,revision,data) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET target_id=excluded.target_id,enabled=excluded.enabled,next_run=excluded.next_run,revision=excluded.revision,data=excluded.data`, j.ID, j.TargetID, j.Enabled, j.NextRun, j.Revision, encoded(j)); err != nil {
			return err
		}
		return audit(tx, actor, "job.save", j.ID, fmt.Sprintf("revision %d", j.Revision))
	})
}

// CreateJob gives the caller the generated ID while retaining transactional validation.
func (s *Store) CreateJob(j Job, actor string) (Job, error) {
	if err := j.Validate(); err != nil {
		return j, err
	}
	j.ID = randomID()
	j.Revision = 1
	sc, _ := schedule(j.Schedule, j.Timezone)
	j.NextRun = sc.Next(time.Now()).UnixMilli()
	err := transaction(s.DB, func(tx *sql.Tx) error {
		if _, e := targetFrom(tx, j.TargetID); e != nil {
			return errors.New("target does not exist")
		}
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); e != nil {
			return e
		}
		if n >= 1000 {
			return errors.New("job limit reached (1000)")
		}
		if _, e := tx.Exec(`INSERT INTO jobs VALUES(?,?,?,?,?,?)`, j.ID, j.TargetID, j.Enabled, j.NextRun, j.Revision, encoded(j)); e != nil {
			return e
		}
		return audit(tx, actor, "job.create", j.ID, "")
	})
	return j, err
}

const runColumns = `id,job_id,target_id,scheduled,created,status,start_before,deadline,attempt,next_attempt,session_id,cancel,resolved,manual,reason,snapshot,stdout,stderr,truncated,exit_code,http_status`

type scanner interface{ Scan(...any) error }

func scanRun(row scanner) (Run, error) {
	var r Run
	var data string
	err := row.Scan(&r.ID, &r.JobID, &r.TargetID, &r.Scheduled, &r.Created, &r.Status, &r.StartBefore, &r.Deadline, &r.Attempt, &r.NextAttempt, &r.SessionID, &r.Cancel, &r.Resolved, &r.Manual, &r.Reason, &data, &r.Stdout, &r.Stderr, &r.Truncated, &r.ExitCode, &r.HTTPStatus)
	if err == nil {
		err = json.Unmarshal([]byte(data), &r.Snapshot)
	}
	return r, err
}
func (s *Store) Run(id string) (Run, error) {
	return scanRun(s.DB.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id=?`, id))
}
func (s *Store) Runs(limit, offset int) ([]Run, error) {
	rows, err := s.DB.Query(`SELECT `+runColumns+` FROM runs ORDER BY created DESC,id LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func insertRun(q querier, j Job, t Target, at, created int64, manual bool, key, status, reason string) (string, error) {
	id := randomID()
	_, err := q.Exec(`INSERT INTO runs(id,job_id,target_id,revision,scheduled,created,status,start_before,manual,reason,snapshot,request_key) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, j.ID, j.TargetID, j.Revision, at, created, status, created+int64(j.Policy.StartGraceSeconds)*1000, manual, reason, encoded(Snapshot{j, t}), key)
	return id, err
}
func (s *Store) Enqueue(id, key, actor string) (string, error) {
	var result string
	err := transaction(s.DB, func(tx *sql.Tx) error {
		if key != "" {
			e := tx.QueryRow(`SELECT id FROM runs WHERE job_id=? AND request_key=?`, id, key).Scan(&result)
			if e == nil {
				return nil
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		j, e := jobFrom(tx, id)
		if e != nil {
			return e
		}
		t, e := targetFrom(tx, j.TargetID)
		if e != nil {
			return e
		}
		var count int
		tx.QueryRow(`SELECT count(*) FROM runs WHERE status IN ('queued','retry_wait')`).Scan(&count)
		if count >= 1000 {
			return errors.New("pending queue is full")
		}
		result, e = insertRun(tx, j, t, nowMS(), nowMS(), true, key, "queued", "")
		if e != nil {
			return e
		}
		return audit(tx, actor, "run.enqueue", result, "")
	})
	return result, err
}
func (s *Store) Backup(dest string) error {
	abs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if _, err = os.Stat(abs); !os.IsNotExist(err) {
		return errors.New("backup destination must not already exist")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_, err = s.DB.ExecContext(ctx, `VACUUM INTO ?`, abs)
	if err == nil {
		err = os.Chmod(abs, 0600)
	}
	return err
}
