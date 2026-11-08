package reacquire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/sirrobot01/decypharr/internal/state"
	"github.com/sirrobot01/decypharr/internal/task"
	"github.com/sirrobot01/decypharr/pkg/arr"
)

const (
	bindingsDatabaseName = "arr_bindings.db"
	jobsDatabaseName     = "reacquire_jobs.db"
)

type JobProgress interface {
	Update(status Status, mutate func(*Job)) error
	UpdateDurable(status Status, mutate func(*Job)) error
}

type Handler interface {
	Reacquire(context.Context, Job, JobProgress) error
}

type ServiceOptions struct {
	Arrs      *arr.Service
	Directory string
	Index     *Index
	Handler   Handler
	// Logger records what the service changes on its own; the zero value logs nothing.
	Logger zerolog.Logger
	// OnNeedsAttention, when set, is called once each time a job moves to needs_attention.
	// It must not block: the service calls it from the worker.
	OnNeedsAttention func(NeedsAttention)
}

type jobKey struct {
	arrName    string
	downloadID string
	entryID    string
	fileID     string
}

type Service struct {
	// g owns everything the service runs: the worker child and the close task. The worker holds the run loop and
	// the retry timers, and stops before g's own tasks so the close task can wait for it.
	g                 *task.Group
	worker            *task.Group
	arrs              *arr.Service
	logger            zerolog.Logger
	onNeedsAttention  func(NeedsAttention)
	index             *Index
	bindingRepository *BindingRepository
	jobRepository     *JobRepository
	wake              chan struct{}
	now               func() time.Time
	// lifecycleMu is the operation guard. Public entry points hold the read lock for their whole duration; Start
	// and the close task take the write lock. Internal helpers never take it: a nested read lock deadlocks behind
	// a waiting writer.
	lifecycleMu sync.RWMutex
	state       serviceState
	// closeErr is what closing the repositories returned; set by the close task, under lifecycleMu.
	closeErr             error
	handlerMu            sync.Mutex
	handler              Handler
	jobsMu               sync.RWMutex
	jobs                 map[string]Job
	activeReacquisitions map[jobKey]string
	// beforeMaintenanceCtx, when set, is called in completeJobFromIndex just before it reads the group's
	// context. It is a seam for tests that hold a public entry point in flight there while Close queues for the
	// write lock; completeJobFromIndex takes no lock, so nothing may block there for good.
	beforeMaintenanceCtx func()
}

func NewService(g *task.Group, options ServiceOptions) (*Service, error) {
	if options.Directory == "" {
		return nil, errors.New("arr service database directory is required")
	}
	bindingRepository, err := OpenBindingRepository(filepath.Join(options.Directory, bindingsDatabaseName))
	if err != nil {
		return nil, err
	}
	jobRepository, err := OpenReacquireJobRepository(filepath.Join(options.Directory, jobsDatabaseName))
	if err != nil {
		_ = bindingRepository.Close()
		return nil, err
	}
	index := options.Index
	if index == nil {
		index = NewIndex()
	}
	service := &Service{
		g:                    g,
		worker:               g.Child("worker"),
		arrs:                 options.Arrs,
		logger:               options.Logger,
		onNeedsAttention:     options.OnNeedsAttention,
		index:                index,
		bindingRepository:    bindingRepository,
		jobRepository:        jobRepository,
		wake:                 make(chan struct{}, 1),
		now:                  func() time.Time { return time.Now().UTC() },
		handler:              options.Handler,
		jobs:                 make(map[string]Job),
		activeReacquisitions: make(map[jobKey]string),
	}
	// The repositories opened above are closed by this task, so a service whose Start never runs still closes.
	if goErr := g.Go("close", service.closeWhenStopped); goErr != nil {
		_ = bindingRepository.Close()
		_ = jobRepository.Close()
		return nil, fmt.Errorf("own the arr service: %w", goErr)
	}
	return service, nil
}

// moveLocked moves the service to state to when serviceSuccessors allows it. The caller holds lifecycleMu.
// A move the table refuses is ignored: the state stays where it was.
func (s *Service) moveLocked(to serviceState) {
	if state.Legal(serviceSuccessors, s.state, to) {
		s.state = to
	}
}

// closeWhenStopped is the service's close task. It waits for the group to stop, then for the worker, then takes
// the operation guard's write lock, so no entry point is still using the repositories when they close.
func (s *Service) closeWhenStopped(ctx context.Context) error {
	<-ctx.Done()
	// Under a rush the group does not wait between its steps, and a group whose context ended on its own has
	// not stopped its children at all: stop the worker here and wait for it explicitly.
	s.worker.Close()
	<-s.worker.Done()
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	s.moveLocked(serviceStopped)
	s.closeErr = errors.Join(s.bindingRepository.Close(), s.jobRepository.Close())
	return nil
}

// Start loads the stores and starts the worker. It is for one call; a later call finds the service running.
// The service runs until its group stops.
func (s *Service) Start(_ context.Context) error {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	switch s.state {
	case serviceRunning:
		return nil
	case serviceFailed, serviceStopped:
		return ErrServiceClosed
	case serviceConstructed:
	}
	if err := s.load(); err != nil {
		s.moveLocked(serviceFailed)
		return err
	}
	if err := s.worker.Go("run", func(ctx context.Context) error {
		s.run(ctx)
		return nil
	}); err != nil {
		return ErrServiceClosed
	}
	s.moveLocked(serviceRunning)
	s.schedulePersistedRetries()
	s.signal()
	return nil
}

// load reads the persisted bindings and jobs and completes the waiting jobs whose replacement is indexed.
// The caller holds lifecycleMu for writing, so nothing here may take it.
func (s *Service) load() error {
	bindings, err := s.bindingRepository.LoadAll()
	if err != nil {
		return fmt.Errorf("load arr bindings: %w", err)
	}
	bindings = newestBindingRows(bindings)
	if replaceAllErr := s.index.replaceAll(bindings); replaceAllErr != nil {
		return replaceAllErr
	}
	jobs, err := s.jobRepository.LoadAll()
	if err != nil {
		return fmt.Errorf("load reacquire jobs: %w", err)
	}
	if loadJobsErr := s.loadJobs(jobs); loadJobsErr != nil {
		return loadJobsErr
	}
	for _, job := range s.Jobs() {
		if job.Status.waiting() {
			if completeJobFromIndexErr := s.completeJobFromIndex(job); completeJobFromIndexErr != nil {
				return fmt.Errorf("reconcile persisted reacquire job %q: %w", job.ID, completeJobFromIndexErr)
			}
		}
	}
	return nil
}

// Close stops the service's group and waits for it, so the repositories are closed when it returns. It is for
// callers outside the manager, which lets the group stop on its own; it must not be called from a task of the
// group (it would wait for itself). It returns what closing the repositories returned.
func (s *Service) Close() error {
	s.g.Close()
	<-s.g.Done()
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return s.closeErr
}

func (s *Service) SetHandler(handler Handler) error {
	release, err := s.beginAnyOperation()
	if err != nil {
		return err
	}
	defer release()
	s.handlerMu.Lock()
	s.handler = handler
	s.handlerMu.Unlock()
	s.signal()
	return nil
}

func (s *Service) Index() *Index {
	return s.index
}

// IndexSummary reports what the binding index holds, per Arr.
func (s *Service) IndexSummary() []ArrSummary {
	return s.index.Summary()
}

// SearchBindings finds indexed files by name, for a person choosing one.
func (s *Service) SearchBindings(arrName, query string, limit int) []Binding {
	return s.index.Search(arrName, query, limit)
}

func (s *Service) Lookup(entryID, fileID string) (Binding, bool) {
	return s.index.Lookup(entryID, fileID)
}

func (s *Service) UpsertBinding(binding Binding) error {
	release, err := s.beginOperation()
	if err != nil {
		return err
	}
	defer release()
	binding.UpdatedAt = s.now()
	if validateErr := binding.validate(); validateErr != nil {
		return fmt.Errorf("upsert arr binding: %w", validateErr)
	}
	previous, collision := s.index.ByArrFile(binding.ArrName, binding.ArrFileID)
	if previous.EntryID == binding.EntryID && previous.EntryFileID == binding.EntryFileID {
		collision = false
	}
	if saveErr := s.bindingRepository.Save(binding); saveErr != nil {
		return saveErr
	}
	if collision {
		if deleteErr := s.bindingRepository.Delete(previous.EntryID, previous.EntryFileID); deleteErr != nil {
			return deleteErr
		}
	}
	if upsertErr := s.index.Upsert(binding); upsertErr != nil {
		return upsertErr
	}
	return s.completeWaitingJobs(binding)
}

func newestBindingRows(bindings []Binding) []Binding {
	byArrFile := make(map[arrFileKey]Binding, len(bindings))
	withoutArrFile := make([]Binding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.ArrFileID <= 0 {
			withoutArrFile = append(withoutArrFile, binding)
			continue
		}
		key := arrFileKey{arrName: binding.ArrName, fileID: binding.ArrFileID}
		current, exists := byArrFile[key]
		if !exists || binding.UpdatedAt.After(current.UpdatedAt) ||
			(binding.UpdatedAt.Equal(current.UpdatedAt) && binding.Generation > current.Generation) {
			byArrFile[key] = binding
		}
	}
	result := make([]Binding, 0, len(withoutArrFile)+len(byArrFile))
	result = append(result, withoutArrFile...)
	for _, binding := range byArrFile {
		result = append(result, binding)
	}
	sortBindings(result)
	return result
}

// Generation is the newest generation stored for arrName.
func (s *Service) Generation(arrName string) uint64 {
	return s.index.Generation(arrName)
}

func (s *Service) ReplaceArrGeneration(arrName string, generation uint64, bindings []Binding) error {
	release, err := s.beginOperation()
	if err != nil {
		return err
	}
	defer release()
	now := s.now()
	prepared := make([]Binding, len(bindings))
	for i, binding := range bindings {
		binding.ArrName = arrName
		binding.Generation = generation
		binding.UpdatedAt = now
		prepared[i] = binding
	}
	if validateUniqueArrFilesErr := validateUniqueArrFiles(prepared); validateUniqueArrFilesErr != nil {
		return validateUniqueArrFilesErr
	}
	if replaceArrGenerationErr := s.bindingRepository.ReplaceArrGeneration(
		arrName,
		generation,
		prepared,
	); replaceArrGenerationErr != nil {
		return replaceArrGenerationErr
	}
	if replaceArrGenerationErr := s.index.ReplaceArrGeneration(
		arrName,
		generation,
		prepared,
	); replaceArrGenerationErr != nil {
		return replaceArrGenerationErr
	}
	return s.completeWaitingJobs(prepared...)
}

func (s *Service) DeleteBinding(entryID, fileID string) error {
	release, err := s.beginOperation()
	if err != nil {
		return err
	}
	defer release()
	if deleteErr := s.bindingRepository.Delete(entryID, fileID); deleteErr != nil {
		return deleteErr
	}
	s.index.DeleteEntryFile(entryID, fileID)
	return nil
}

// beginOperation is the guard of a public entry point that needs a running service. The caller releases the read
// lock it returns when the operation ends.
func (s *Service) beginOperation() (func(), error) {
	release, err := s.beginAnyOperation()
	if err != nil {
		return nil, err
	}
	if s.state == serviceConstructed {
		release()
		return nil, ErrServiceNotStarted
	}
	return release, nil
}

// beginAnyOperation is beginOperation for an entry point that may also run before Start: it refuses only a
// failed or stopped service, or one whose group is stopping.
func (s *Service) beginAnyOperation() (func(), error) {
	s.lifecycleMu.RLock()
	if s.state == serviceFailed || s.state == serviceStopped || s.g.Context().Err() != nil {
		s.lifecycleMu.RUnlock()
		return nil, ErrServiceClosed
	}
	return s.lifecycleMu.RUnlock, nil
}

func (s *Service) currentHandler() Handler {
	s.handlerMu.Lock()
	defer s.handlerMu.Unlock()
	return s.handler
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) Ready() bool {
	s.lifecycleMu.RLock()
	defer s.lifecycleMu.RUnlock()
	return s.state == serviceRunning && s.g.State() == task.Running
}
