package app

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestClaimJobs: SKIP LOCKED разводит воркеров, ключ разводит дубли постановки.
func TestClaimJobs(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	const total = 10
	for i := range total {
		key := fmt.Sprintf("%s:claim-test:%d", JobNotify, i)
		if err := PutJob(ctx, pool, JobNotify, key, casePayload{CaseID: key}); err != nil {
			t.Fatalf("put job %d: %v", i, err)
		}
	}

	repeated := fmt.Sprintf("%s:claim-test:0", JobNotify)
	if err := PutJob(ctx, pool, JobNotify, repeated, casePayload{CaseID: repeated}); err != nil {
		t.Fatalf("put job again: %v", err)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&queued); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if queued != total {
		t.Fatalf("повтор того же key дал вторую работу: работ %d, ожидалось %d", queued, total)
	}

	var mu sync.Mutex
	claimed := make(map[int64]int, total)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				jobs, err := ClaimJobs(ctx, pool, claimLimit)
				if err != nil {
					t.Errorf("claim jobs: %v", err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				mu.Lock()
				for _, job := range jobs {
					claimed[job.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != total {
		t.Errorf("захвачено работ %d, ожидалось %d", len(claimed), total)
	}
	for id, times := range claimed {
		if times != 1 {
			t.Errorf("работа %d захвачена %d раз", id, times)
		}
	}
}
