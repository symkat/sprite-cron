package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

type Scheduler struct {
	Store     *Store
	Runner    Runner
	Workers   int
	PerTarget int
	Disabled  bool
	mu        sync.Mutex
	active    map[string]context.CancelFunc
	wg        sync.WaitGroup
	Heartbeat atomic.Int64
	Failed    atomic.Bool
}

func NewScheduler(s *Store, r Runner, workers, perTarget int) *Scheduler {
	return &Scheduler{Store: s, Runner: r, Workers: workers, PerTarget: perTarget, active: map[string]context.CancelFunc{}}
}
func (s *Scheduler) Recover() error {
	return transaction(s.Store.DB, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT ` + runColumns + ` FROM runs WHERE status IN ('dispatching','running','recovering')`)
		if err != nil {
			return err
		}
		list := []Run{}
		for rows.Next() {
			r, e := scanRun(rows)
			if e != nil {
				rows.Close()
				return e
			}
			list = append(list, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range list {
			status := "unknown"
			reason := "scheduler restarted; dispatch or completion uncertain"
			if r.SessionID != "" && (r.Snapshot.Job.Execution.Type == "exec" || r.Snapshot.Job.Execution.Type == "shell") {
				status = "recovering"
				reason = "reattaching after restart"
			}
			if _, err = tx.Exec(`UPDATE runs SET status=?,reason=? WHERE id=?`, status, reason, r.ID); err != nil {
				return err
			}
			if status == "unknown" {
				if _, err = tx.Exec(`UPDATE attempts SET status='unknown',reason=?,finished=? WHERE run_id=? AND number=?`, reason, nowMS(), r.ID, r.Attempt); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func (s *Scheduler) Materialize(now time.Time) error {
	return transaction(s.Store.DB, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT data FROM jobs WHERE enabled=1 AND next_run<=? ORDER BY next_run LIMIT 100`, now.UnixMilli())
		if err != nil {
			return err
		}
		jobs := []Job{}
		for rows.Next() {
			var data string
			rows.Scan(&data)
			var j Job
			if err = decode(data, &j); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, j)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			sc, err := schedule(j.Schedule, j.Timezone)
			if err != nil {
				return err
			}
			first := time.UnixMilli(j.NextRun)
			last := first
			next := sc.Next(last)
			count := 1
			for !next.IsZero() && !next.After(now) && count < 10000 {
				last = next
				next = sc.Next(last)
				count++
			}
			if next.IsZero() {
				return errors.New("schedule exhausted")
			}
			t, err := targetFrom(tx, j.TargetID)
			if err != nil {
				return err
			}
			status, reason := "queued", ""
			tooOld := now.Sub(last) > time.Duration(j.Policy.StartGraceSeconds)*time.Second
			if count >= 10000 && !next.After(now) {
				status = "skipped"
				reason = "bounded backlog scan: old occurrences discarded"
			} else if j.Policy.Misfire == "skip" && tooOld {
				status = "skipped"
				reason = "missed start grace"
			}
			var pending int
			if err = tx.QueryRow(`SELECT count(*) FROM runs WHERE status IN ('queued','retry_wait','recovering')`).Scan(&pending); err != nil {
				return err
			}
			if pending >= 1000 {
				status = "skipped"
				reason = "pending queue full"
			}
			if count > 1 {
				if reason != "" {
					reason += "; "
				}
				reason += fmt.Sprintf("%d occurrences from %s through %s; latest represents the window", count, first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339))
			}
			if _, err = insertRun(tx, j, t, last.UnixMilli(), now.UnixMilli(), false, "", status, reason); err != nil {
				return err
			}
			j.NextRun = next.UnixMilli()
			if _, err = tx.Exec(`UPDATE jobs SET next_run=?,data=? WHERE id=?`, j.NextRun, encoded(j), j.ID); err != nil {
				return err
			}
		}
		return nil
	})
}
func decode(data string, v any) error { return json.Unmarshal([]byte(data), v) }
func (s *Scheduler) claim(now time.Time) (Run, error) {
	var result Run
	err := transaction(s.Store.DB, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT `+runColumns+` FROM runs WHERE status IN ('queued','retry_wait','recovering') AND next_attempt<=? ORDER BY created LIMIT 1000`, now.UnixMilli())
		if err != nil {
			return err
		}
		list := []Run{}
		for rows.Next() {
			r, e := scanRun(rows)
			if e != nil {
				rows.Close()
				return e
			}
			list = append(list, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range list {
			if r.Cancel && r.Status != "recovering" {
				_, err = tx.Exec(`UPDATE runs SET status=CASE WHEN status='retry_wait' AND EXISTS(SELECT 1 FROM attempts WHERE run_id=runs.id AND number=runs.attempt AND status='unknown') THEN 'unknown' ELSE 'cancelled' END,reason='cancelled before dispatch; previous uncertain attempts remain unknown' WHERE id=?`, r.ID)
				if err != nil {
					return err
				}
				continue
			}
			recovery := r.Status == "recovering"
			if !recovery && r.Status == "queued" && r.StartBefore < now.UnixMilli() {
				if _, err = tx.Exec(`UPDATE runs SET status='skipped',reason='dispatch grace expired' WHERE id=?`, r.ID); err != nil {
					return err
				}
				continue
			}
			var active int
			if err = tx.QueryRow(`SELECT count(*) FROM runs WHERE target_id=? AND status IN ('running','dispatching')`, r.TargetID).Scan(&active); err != nil {
				return err
			}
			if active >= s.PerTarget {
				continue
			}
			if !recovery && r.Snapshot.Job.Policy.Overlap == "forbid" {
				var blocking int
				err = tx.QueryRow(`SELECT count(*) FROM runs WHERE job_id=? AND id<>? AND (status IN ('running','dispatching','retry_wait','recovering') OR (status='unknown' AND resolved=0))`, r.JobID, r.ID).Scan(&blocking)
				if err != nil {
					return err
				}
				if blocking > 0 {
					if _, err = tx.Exec(`UPDATE runs SET status='skipped',reason='overlap prevented' WHERE id=?`, r.ID); err != nil {
						return err
					}
					continue
				}
			}
			if !recovery {
				r.Attempt++
				r.SessionID = ""
				r.Deadline = now.Add(time.Duration(r.Snapshot.Job.Policy.TimeoutSeconds) * time.Second).UnixMilli()
				if _, err = tx.Exec(`INSERT INTO attempts(run_id,number,started,status) VALUES(?,?,?,'dispatching')`, r.ID, r.Attempt, now.UnixMilli()); err != nil {
					return err
				}
			}
			if _, err = tx.Exec(`UPDATE runs SET status='dispatching',attempt=?,deadline=?,session_id=?,stdout='',stderr='',truncated=0,exit_code=NULL,http_status=0 WHERE id=?`, r.Attempt, r.Deadline, r.SessionID, r.ID); err != nil {
				return err
			}
			result = r
			result.Status = "dispatching"
			return nil
		}
		return nil
	})
	if err == nil && result.ID == "" {
		err = sql.ErrNoRows
	}
	return result, err
}
func (s *Scheduler) Launch(ctx context.Context, r Run) {
	execCtx, cancel := context.WithDeadline(ctx, time.UnixMilli(r.Deadline))
	if r.Cancel {
		cancel()
	}
	s.mu.Lock()
	s.active[r.ID] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer func() { s.mu.Lock(); delete(s.active, r.ID); s.mu.Unlock() }()
		result := s.Runner.Execute(execCtx, r, func(id string) error {
			err := transaction(s.Store.DB, func(tx *sql.Tx) error {
				if _, err := tx.Exec(`UPDATE runs SET status='running',session_id=? WHERE id=?`, id, r.ID); err != nil {
					return err
				}
				_, err := tx.Exec(`UPDATE attempts SET session_id=?,status='running' WHERE run_id=? AND number=?`, id, r.ID, r.Attempt)
				return err
			})
			if err != nil {
				s.Failed.Store(true)
			}
			return err
		})
		err := s.finish(r, result)
		if err != nil {
			s.Failed.Store(true)
			slog.Error("could not persist run result; dispatch stopped", "run_id", r.ID)
		}
	}()
}
func (s *Scheduler) finish(r Run, result Result) error {
	return transaction(s.Store.DB, func(tx *sql.Tx) error {
		var cancelled bool
		if err := tx.QueryRow(`SELECT cancel FROM runs WHERE id=?`, r.ID).Scan(&cancelled); err != nil {
			return err
		}
		status := result.Status
		next := int64(0)
		p := r.Snapshot.Job.Policy
		if !cancelled && result.Retryable && p.RetrySafe && r.Attempt < p.MaxAttempts && time.Now().Before(time.UnixMilli(r.Created).Add(time.Duration(p.TimeoutSeconds*p.MaxAttempts+1800)*time.Second)) {
			delay := time.Duration(5*(1<<uint(r.Attempt-1)))*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
			if result.RetryAfter > delay {
				delay = result.RetryAfter
			}
			status = "retry_wait"
			next = time.Now().Add(delay).UnixMilli()
		}
		if _, err := tx.Exec(`UPDATE attempts SET finished=?,status=?,reason=?,stdout=?,stderr=?,truncated=?,exit_code=?,http_status=? WHERE run_id=? AND number=?`, nowMS(), result.Status, result.Reason, result.Stdout, result.Stderr, result.Truncated, result.ExitCode, result.HTTPStatus, r.ID, r.Attempt); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE runs SET status=?,reason=?,next_attempt=?,stdout=?,stderr=?,truncated=?,exit_code=?,http_status=? WHERE id=?`, status, result.Reason, next, result.Stdout, result.Stderr, result.Truncated, result.ExitCode, result.HTTPStatus, r.ID)
		return err
	})
}
func (s *Scheduler) cancellations() error {
	rows, err := s.Store.DB.Query(`SELECT id FROM runs WHERE cancel=1 AND status IN ('running','dispatching')`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		if cancel := s.active[id]; cancel != nil {
			cancel()
		}
	}
	return nil
}
func (s *Scheduler) Loop(ctx context.Context) error {
	if err := s.Recover(); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		if err := s.Store.DB.Ping(); err != nil {
			s.Failed.Store(true)
		}
		if !s.Failed.Load() {
			s.Heartbeat.Store(nowMS())
			if err := s.cancellations(); err != nil {
				s.Failed.Store(true)
			}
			if !s.Disabled {
				if err := s.Materialize(time.Now()); err != nil {
					s.Failed.Store(true)
					slog.Error("scheduler materialization failed")
				}
				for !s.Failed.Load() {
					s.mu.Lock()
					n := len(s.active)
					s.mu.Unlock()
					if n >= s.Workers {
						break
					}
					r, err := s.claim(time.Now())
					if errors.Is(err, sql.ErrNoRows) {
						break
					}
					if err != nil {
						s.Failed.Store(true)
						slog.Error("scheduler claim failed")
						break
					}
					s.Launch(ctx, r)
				}
			}
			if time.Since(lastPrune) > time.Minute {
				if err := s.Prune(); err != nil {
					slog.Error("retention cleanup failed")
				}
				lastPrune = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-ticker.C:
		}
	}
}
func (s *Scheduler) Prune() error {
	return transaction(s.Store.DB, func(tx *sql.Tx) error {
		cutoff := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
		for _, query := range []string{`DELETE FROM sessions WHERE expires<?`, `DELETE FROM audit WHERE at<?`, `DELETE FROM runs WHERE created<? AND status IN ('succeeded','failed','skipped','cancelled','timed_out')`} {
			if _, err := tx.Exec(query, cutoff); err != nil {
				return err
			}
		}
		// Trim oldest closed-run output until the shared run/attempt output budget is met.
		var used int64
		if err := tx.QueryRow(`SELECT COALESCE((SELECT sum(length(stdout)+length(stderr)) FROM runs),0)+COALESCE((SELECT sum(length(stdout)+length(stderr)) FROM attempts),0)`).Scan(&used); err != nil {
			return err
		}
		if used > 512*1024*1024 {
			rows, err := tx.Query(`SELECT id,length(stdout)+length(stderr)+COALESCE((SELECT sum(length(stdout)+length(stderr)) FROM attempts WHERE run_id=runs.id),0) AS bytes FROM runs WHERE status NOT IN ('running','dispatching','recovering') ORDER BY created`)
			if err != nil {
				return err
			}
			type output struct {
				id    string
				bytes int64
			}
			var victims []output
			for rows.Next() {
				var item output
				if err = rows.Scan(&item.id, &item.bytes); err != nil {
					rows.Close()
					return err
				}
				if item.bytes > 0 {
					victims = append(victims, item)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			for _, item := range victims {
				if used <= 512*1024*1024 {
					break
				}
				if _, err = tx.Exec(`UPDATE runs SET stdout='',stderr='',truncated=1 WHERE id=?`, item.id); err != nil {
					return err
				}
				if _, err = tx.Exec(`UPDATE attempts SET stdout='',stderr='',truncated=1 WHERE run_id=?`, item.id); err != nil {
					return err
				}
				used -= item.bytes
			}
		}
		return nil
	})
}
