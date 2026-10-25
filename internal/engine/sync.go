// Package engine connects the source readers and the index: it brings the
// index up to date and answers search, list and read requests.
package engine

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/emmaneugene/session-search/internal/index"
	"github.com/emmaneugene/session-search/internal/model"
	"github.com/emmaneugene/session-search/internal/source"
)

// SyncStats reports what one sync did for one agent.
type SyncStats struct {
	Agent   string `json:"agent"`
	Files   int    `json:"files"`
	Changed int    `json:"reindexed"`
	Removed int    `json:"removed"`
	Failed  int    `json:"failed"`
}

// retryFailed is how long a file that failed to parse waits before it's
// tried again without having changed. A change retries it at once.
const retryFailed = 10 * time.Minute

type work struct {
	src source.Source
	f   source.File
}

type parsed struct {
	fingerprint string
	session     model.Session
	entries     []model.Entry
	err         error
}

// Sync reindexes every session file whose fingerprint changed and drops
// sessions whose files are gone. progress, if not nil, gets a line now and
// then during a long build.
func Sync(ix *index.Index, env source.Env, srcs []source.Source, progress io.Writer) ([]SyncStats, error) {
	p, stats, err := makePlan(ix, env, srcs)
	if err != nil {
		return nil, err
	}
	if len(p.todo) == 0 && len(p.gone) == 0 {
		return stats, nil
	}
	// Another process may be doing the same work: wait for it, then plan
	// again, because it may have finished everything.
	if err := ix.Lock(); err != nil {
		return nil, err
	}
	defer ix.Unlock()
	p, stats, err = makePlan(ix, env, srcs)
	if err != nil {
		return nil, err
	}
	if err := apply(ix, env, p, stats, progress); err != nil {
		return nil, err
	}
	return stats, ix.SetMeta("last_sync", time.Now().UTC().Format(time.RFC3339))
}

type syncPlan struct {
	todo []work
	gone map[string][]string // agent -> files
}

func makePlan(ix *index.Index, env source.Env, srcs []source.Source) (syncPlan, []SyncStats, error) {
	p := syncPlan{gone: map[string][]string{}}
	var stats []SyncStats
	for _, src := range srcs {
		files, err := src.Discover(env)
		if err != nil {
			return p, nil, fmt.Errorf("%s: discover: %w", src.Agent(), err)
		}
		known, err := ix.Fingerprints(src.Agent())
		if err != nil {
			return p, nil, err
		}
		st := SyncStats{Agent: src.Agent(), Files: len(files)}
		for _, f := range files {
			k, seen := known[f.Path]
			delete(known, f.Path)
			if seen && k.Fingerprint == f.Fingerprint {
				continue
			}
			if seen && k.FailedFingerprint == f.Fingerprint && time.Since(k.FailedAt) < retryFailed {
				continue // failed recently and unchanged since
			}
			p.todo = append(p.todo, work{src: src, f: f})
			st.Changed++
		}
		for file := range known {
			p.gone[src.Agent()] = append(p.gone[src.Agent()], file)
			st.Removed++
		}
		stats = append(stats, st)
	}
	return p, stats, nil
}

func apply(ix *index.Index, env source.Env, p syncPlan, stats []SyncStats, progress io.Writer) error {
	jobs := make(chan work)
	results := make(chan parsed, 8)
	var wg sync.WaitGroup
	for range max(1, runtime.NumCPU()-1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w := range jobs {
				s, entries, err := w.src.Parse(env, w.f.Path)
				if s.Agent == "" {
					s.Agent = w.src.Agent()
				}
				s.File = w.f.Path
				results <- parsed{fingerprint: w.f.Fingerprint, session: s, entries: entries, err: err}
			}
		}()
	}
	go func() {
		for _, w := range p.todo {
			jobs <- w
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	tx, err := ix.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	for agent, files := range p.gone {
		for _, f := range files {
			if err := tx.Delete(f); err != nil {
				return fmt.Errorf("%s: drop %s: %w", agent, f, err)
			}
		}
	}

	failed := map[string]int{}
	done, total := 0, len(p.todo)
	lastCommit, lastReport := time.Now(), time.Now()
	for r := range results {
		if r.err != nil {
			failed[r.session.Agent]++
			if err := tx.Failed(r.session.Agent, r.session.File, r.fingerprint, r.err, time.Now()); err != nil {
				return fmt.Errorf("index %s: %w", r.session.File, err)
			}
		} else if err := tx.Put(r.session, r.fingerprint, r.entries); err != nil {
			return fmt.Errorf("index %s: %w", r.session.File, err)
		}
		done++
		// Commit now and then, so a long first build keeps its progress if
		// it's interrupted.
		if time.Since(lastCommit) > 2*time.Second {
			if err := tx.Commit(); err != nil {
				return err
			}
			if tx, err = ix.Begin(); err != nil {
				return err
			}
			lastCommit = time.Now()
		}
		if progress != nil && total > 50 && time.Since(lastReport) > 2*time.Second {
			fmt.Fprintf(progress, "session-search: indexing %d/%d sessions\n", done, total)
			lastReport = time.Now()
		}
	}
	err = tx.Commit()
	tx = nil
	for i := range stats {
		stats[i].Failed = failed[stats[i].Agent]
	}
	return err
}
