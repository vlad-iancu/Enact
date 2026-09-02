package enactcrawlorchestrator

import (
	"context"
	"time"

	"github.com/google/uuid"

	"enact/internal/crawls"
	"enact/internal/logging"
	"enact/internal/queue"
)

// Scheduler queues crawls whose next run is due.
//
// A ticker rather than a cron library, matching the platform's existing
// sweeps (the token refresh in enact-external-identities, the tool-cache
// refresh in enact-tool-registry). A crawl stores an interval and the
// timestamp of its next run; the sweep finds the ones that are owed, queues
// them, and moves the timestamp forward.
//
// Single-replica, like those sweeps: there is no leader election anywhere in
// the platform. Two orchestrators would each queue the same due crawl, and
// the runner's status check makes the duplicate harmless — the second run
// finds the record no longer queued and stops — but it is wasteful, so this
// assumes one.
type Scheduler struct {
	crawls   *crawls.Repository
	runs     *crawls.RunRepository
	producer *queue.Producer
	batch    int
	logger   *logging.Logger
}

// Loop sweeps until ctx is cancelled.
func (s *Scheduler) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	s.logger.Info("crawl scheduler started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("crawl scheduler stopped")
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

// sweep queues every crawl that is due.
//
// Errors never end the loop: an unreachable OpenSearch or a crawl that fails
// to queue is a reason to try again next tick, not a reason to stop
// scheduling everything else.
func (s *Scheduler) sweep(ctx context.Context) {
	now := time.Now().UTC()
	due, err := s.crawls.Due(ctx, now, s.batch)
	if err != nil {
		s.logger.Error("failed to look for due crawls", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	s.logger.Info("due crawls found", "count", len(due))
	for _, crawl := range due {
		if err := s.queue(ctx, crawl, now); err != nil {
			s.logger.Error("failed to queue a due crawl", "crawl_id", crawl.ID, "err", err)
		}
	}
}

// queue writes a run record, advances the crawl's schedule, and publishes.
func (s *Scheduler) queue(ctx context.Context, crawl crawls.Crawl, now time.Time) error {
	run := crawls.Run{
		ID:      uuid.NewString(),
		CrawlID: crawl.ID,
		// The run acts as the crawl's owner, which is whose knowledge base it
		// writes into. A schedule has no user of its own.
		UserID:         crawl.UserID,
		OrganizationID: crawl.OrganizationID,
		Status:         crawls.StatusQueued,
		QueuedAt:       now,
	}
	if err := s.runs.Save(ctx, run); err != nil {
		return err
	}

	// The schedule advances BEFORE publishing, and is saved even if
	// publishing then fails. Otherwise a crawl that cannot be queued stays
	// permanently due and every sweep tries it again, turning one broken
	// crawl into a tight loop against Redis.
	crawl.NextRunAt = nextRun(crawl, now)
	crawl.UpdatedAt = now
	if err := s.crawls.Update(ctx, crawl); err != nil {
		// Not fatal to the run: the work is queued below either way, and the
		// worst case is that this crawl is picked up again next sweep.
		s.logger.Error("failed to advance the crawl schedule", "crawl_id", crawl.ID, "err", err)
	}

	if err := s.producer.PublishCrawlRun(ctx, queue.CrawlRunMessage{RunID: run.ID}); err != nil {
		run.Status = crawls.StatusFailed
		run.Error = "the run could not be queued"
		run.FinishedAt = time.Now().UTC()
		if saveErr := s.runs.Save(ctx, run); saveErr != nil {
			s.logger.Error("failed to record the queueing failure", "run_id", run.ID, "err", saveErr)
		}
		return err
	}
	s.logger.Info("crawl run queued by schedule",
		"crawl_id", crawl.ID, "run_id", run.ID, "next_run_at", crawl.NextRunAt)
	return nil
}

// nextRun is when a crawl should run after this one.
//
// Measured from now rather than from the previous scheduled time, so a crawl
// that was late (the service was down, the sweep was slow) does not
// immediately fire again to "catch up" — for a crawler, catching up means
// hammering somebody's site, and the missed runs have no value to recover.
func nextRun(crawl crawls.Crawl, now time.Time) time.Time {
	interval := crawl.Interval()
	if interval <= 0 {
		// Unscheduled: push it far enough out that the due query stops
		// matching. Disabling scheduling by clearing the interval must not
		// leave the crawl permanently due.
		return now.Add(100 * 365 * 24 * time.Hour)
	}
	return now.Add(interval)
}
