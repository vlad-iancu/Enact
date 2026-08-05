package enacttests

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"enact/internal/enacttests/utils"
	"enact/internal/logging"
)

// Test outcome statuses reported by GET /v1/execution.
const (
	statusSuccess = "SUCCESS"
	statusFailed  = "FAILED"
)

// caseResult is one completed test case.
type caseResult struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Error carries the first failure message of a FAILED case so a result
	// is diagnosable without grepping logs.
	Error string `json:"error,omitempty"`
}

// execution tracks one asynchronous test run.
type execution struct {
	id      string
	total   int
	started time.Time

	mu        sync.Mutex
	completed []caseResult
}

// snapshot returns the completed results plus the pending count.
func (e *execution) snapshot() ([]caseResult, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]caseResult, len(e.completed))
	copy(out, e.completed)
	return out, e.total - len(out)
}

func (e *execution) record(r caseResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.completed = append(e.completed, r)
}

// Runner selects, executes, and tracks test runs over an explicitly
// supplied list of case factories. Executions are kept in memory; the
// service is a dev tool, restarts discard history.
type Runner struct {
	env    *utils.Env
	cases  map[string]utils.Factory
	logger *logging.Logger

	mu         sync.RWMutex
	executions map[string]*execution
}

// NewRunner indexes the factories by case name. Duplicate names are a
// programming error surfaced at startup.
func NewRunner(env *utils.Env, factories []utils.Factory, logger *logging.Logger) (*Runner, error) {
	cases := make(map[string]utils.Factory, len(factories))
	for _, factory := range factories {
		name := factory().Name()
		if _, dup := cases[name]; dup {
			return nil, fmt.Errorf("enacttests: duplicate test case %q", name)
		}
		cases[name] = factory
	}
	return &Runner{env: env, cases: cases, logger: logger, executions: map[string]*execution{}}, nil
}

// CaseCount reports how many cases are registered.
func (r *Runner) CaseCount() int { return len(r.cases) }

// sortedNames returns the registered case names, sorted for stable runs.
func sortedNames(cases map[string]utils.Factory) []string {
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// maxWorkers caps the worker pool so a typo cannot spawn thousands of
// concurrent test goroutines against the live services.
const maxWorkers = 32

// Start launches an asynchronous run of every registered case matching the
// tests regex and not matching the skip regex, executed by numWorkers
// concurrent workers. It returns the execution id immediately.
func (r *Runner) Start(tests, skip string, numWorkers int) (string, int, error) {
	if tests == "" {
		tests = ".*"
	}
	testsRe, err := regexp.Compile(tests)
	if err != nil {
		return "", 0, fmt.Errorf("invalid tests regex: %v", err)
	}
	var skipRe *regexp.Regexp
	if skip != "" {
		if skipRe, err = regexp.Compile(skip); err != nil {
			return "", 0, fmt.Errorf("invalid skip regex: %v", err)
		}
	}

	var selected []string
	for _, name := range sortedNames(r.cases) {
		if !testsRe.MatchString(name) {
			continue
		}
		if skipRe != nil && skipRe.MatchString(name) {
			continue
		}
		selected = append(selected, name)
	}

	if numWorkers <= 0 {
		numWorkers = 4
	}
	if numWorkers > maxWorkers {
		numWorkers = maxWorkers
	}
	if numWorkers > len(selected) && len(selected) > 0 {
		numWorkers = len(selected)
	}

	exec := &execution{id: uuid.NewString(), total: len(selected), started: time.Now().UTC()}
	r.mu.Lock()
	r.executions[exec.id] = exec
	r.mu.Unlock()

	logger := r.logger.WithFields("exec_id", exec.id)
	logger.Info("test execution started", "selected", len(selected), "workers", numWorkers, "tests", tests, "skip", skip)

	jobs := make(chan string)
	for i := 0; i < numWorkers; i++ {
		go r.worker(exec, jobs, logger)
	}
	go func() {
		for _, name := range selected {
			jobs <- name
		}
		close(jobs)
	}()

	return exec.id, len(selected), nil
}

// Get returns the execution with the given id.
func (r *Runner) Get(id string) (*execution, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.executions[id]
	return e, ok
}

func (r *Runner) worker(exec *execution, jobs <-chan string, logger *logging.Logger) {
	for name := range jobs {
		exec.record(r.runCase(name, logger))
	}
}

// runCase executes one test case through its Setup -> Run -> TearDown
// lifecycle on a fresh instance. A failed or aborted Setup skips Run;
// TearDown always executes so fixtures cannot leak.
func (r *Runner) runCase(name string, logger *logging.Logger) caseResult {
	tc := r.cases[name]()
	t := utils.NewT(name, r.env, context.Background(), logger)
	start := time.Now()
	logger.Info("test case started", "case", name)

	utils.RunPhase(t, "setup", tc.Setup)
	if t.Failed() {
		logger.Warn("setup failed; skipping run", "case", name)
	} else {
		utils.RunPhase(t, "run", tc.Run)
	}
	utils.RunPhase(t, "teardown", tc.TearDown)

	result := caseResult{Name: name, Status: statusSuccess}
	if t.Failed() {
		result.Status = statusFailed
		result.Error = t.Failure()
	}
	logger.Info("test case finished", "case", name, "status", result.Status, "duration_ms", time.Since(start).Milliseconds())
	return result
}
