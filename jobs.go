package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Job is the execution record of one pack/install task: status + timestamped
// log lines.
type Job struct {
	mu       sync.Mutex
	ID       string `json:"id"`
	Kind     string `json:"kind"`   // pack | install
	Status   string `json:"status"` // running | success | failed
	Err      string `json:"err,omitempty"`
	Created  time.Time
	Finished time.Time
	lines    []string
}

func (j *Job) Logf(format string, args ...any) {
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
	j.mu.Lock()
	j.lines = append(j.lines, line)
	j.mu.Unlock()
}

// Write implements io.Writer: command output is appended to the log line by
// line (streamed from runner.Run).
func (j *Job) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		j.mu.Lock()
		j.lines = append(j.lines, line)
		j.mu.Unlock()
	}
	return len(p), nil
}

func (j *Job) finish(status, errMsg string) {
	j.mu.Lock()
	j.Status = status
	j.Err = errMsg
	j.Finished = time.Now()
	dur := j.Finished.Sub(j.Created)
	j.lines = append(j.lines, fmt.Sprintf("[%s] %s %s, total time %s",
		j.Finished.Format("15:04:05"), map[bool]string{true: "job finished", false: "job failed"}[status == "success"], status, fmtDuration(dur)))
	j.mu.Unlock()
}

// Times returns the start/end times (end is the zero value until finished).
func (j *Job) Times() (start, end time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Created, j.Finished
}

// fmtDuration formats a duration as mm:ss or h:mm:ss.
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	h, m, s := total/3600, total%3600/60, total%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// errString reads the failure reason thread-safely.
func (j *Job) errString() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Err
}

// Snapshot returns the log lines after offset plus the current status, for
// frontend polling.
func (j *Job) Snapshot(offset int) (status string, lines []string, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if offset < 0 || offset > len(j.lines) {
		offset = 0
	}
	return j.Status, j.lines[offset:], len(j.lines)
}

type JobStore struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  int
}

func NewJobStore() *JobStore {
	return &JobStore{jobs: map[string]*Job{}}
}

func (s *JobStore) Create(kind string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	j := &Job{
		ID:      fmt.Sprintf("%s-%d-%d", kind, time.Now().Unix(), s.seq),
		Kind:    kind,
		Status:  "running",
		Created: time.Now(),
	}
	s.jobs[j.ID] = j
	return j
}

func (s *JobStore) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j, ok
}
