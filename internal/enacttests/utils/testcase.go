package utils

// TestCase is one integration test. The runner drives the lifecycle:
//
//	Setup    — create the fixtures the test needs; state lives on the
//	           case struct. A failed setup skips Run.
//	Run      — the behaviour under test and its assertions.
//	TearDown — remove everything Setup/Run created. ALWAYS runs, even
//	           when an earlier phase aborted, so cases must make it
//	           tolerant (guard empty ids, accept 404s).
//
// Implementations are stateful (fixtures held on the struct between
// phases), so the registry stores factories and every execution gets a
// fresh instance.
type TestCase interface {
	// Name identifies the case; execution requests select by regex on it.
	// Convention: Test<Area>_<What>, so "TestAgentManagement" matches a
	// whole area.
	Name() string
	Setup(t *T)
	Run(t *T)
	TearDown(t *T)
}

// Factory produces a fresh instance of one test case.
type Factory func() TestCase

// BaseCase provides no-op Setup and TearDown so cases without fixtures only
// implement Name and Run. Embed it:
//
//	type myCase struct{ utils.BaseCase }
type BaseCase struct{}

func (BaseCase) Setup(*T)    {}
func (BaseCase) TearDown(*T) {}
