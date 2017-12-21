package pool

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henrylee2cn/goutil/coarsetime"
)

type (
	// Worker woker interface
	// Note: Worker can not be implemented using empty structures(struct{})!
	Worker interface {
		Health() bool
		Close() error
	}
	// Workshop working workshop
	Workshop struct {
		addFn           func() (*workerInfo, error)
		maxQuota        int
		maxIdleDuration time.Duration
		infos           map[Worker]*workerInfo
		mostFree        *workerInfo
		stats           *WorkshopStats
		statsReader     atomic.Value
		lock            sync.RWMutex
		wg              sync.WaitGroup
		closeCh         chan struct{}
	}
	workerInfo struct {
		worker     Worker
		jobNum     int32
		idleExpire time.Time
		wg         sync.WaitGroup
	}
	// WorkshopStats workshop stats
	WorkshopStats struct {
		Worker    int32
		Idle      int32
		Created   uint64
		Hire      uint64
		Fire      uint64
		Doing     int32
		MostUsed  int32
		LeastUsed int32
	}
)

const (
	defaultWorkerMaxQuota        = 64
	defaultWorkerMaxIdleDuration = 3 * time.Minute
)

var (
	// ErrWorkshopClosed error: 'workshop is closed'
	ErrWorkshopClosed = fmt.Errorf("%s", "workshop is closed")
)

// NewWorkshop creates a new workshop.
// If maxQuota<=0, will use default value.
// If maxIdleDuration<=0, will use default value.
// Note: Worker can not be implemented using empty structures(struct{})!
func NewWorkshop(maxQuota int, maxIdleDuration time.Duration, newWorkerFunc func() (Worker, error)) *Workshop {
	if maxQuota <= 0 {
		maxQuota = defaultWorkerMaxQuota
	}
	if maxIdleDuration <= 0 {
		maxIdleDuration = defaultWorkerMaxIdleDuration
	}
	w := new(Workshop)
	w.stats = new(WorkshopStats)
	w.writeStats()
	w.maxQuota = maxQuota
	w.maxIdleDuration = maxIdleDuration
	w.infos = make(map[Worker]*workerInfo, maxQuota)
	w.closeCh = make(chan struct{})
	w.addFn = func() (info *workerInfo, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("%v", p)
			}
		}()
		worker, err := newWorkerFunc()
		if err != nil {
			return nil, err
		}
		info = &workerInfo{
			worker: worker,
			wg:     w.wg,
		}
		w.infos[worker] = info
		w.stats.Created++
		w.stats.Worker++
		return info, err
	}
	go w.gc()
	return w
}

// Callback assigns a healthy worker to execute the function.
func (w *Workshop) Callback(fn func(Worker) error) error {
	select {
	case <-w.closeCh:
		return ErrWorkshopClosed
	default:
	}
	w.lock.Lock()
	info, err := w.hireLocked()
	w.writeStats()
	w.lock.Unlock()
	if err != nil {
		return err
	}
	worker := info.worker
	defer func() {
		w.lock.Lock()
		_, ok := w.infos[worker]
		if !ok {
			worker.Close()
		} else {
			w.fireLocked(info)
			w.writeStats()
		}
		w.lock.Unlock()
	}()
	return fn(worker)
}

// Close wait for all the work to be completed and close the workshop.
func (w *Workshop) Close() {
	select {
	case <-w.closeCh:
		return
	default:
		close(w.closeCh)
	}
	w.wg.Wait()
	w.lock.Lock()
	defer w.lock.Unlock()
	for _, info := range w.infos {
		info.worker.Close()
	}
	w.infos = nil
	w.stats.Idle = 0
	w.stats.Worker = 0
	w.refreshLocked()
}

// Fire marks the worker to reduce a job.
// If the worker does not belong to the workshop, close the worker.
func (w *Workshop) Fire(worker Worker) {
	w.lock.Lock()
	info, ok := w.infos[worker]
	if !ok {
		if worker != nil {
			worker.Close()
		}
		w.lock.Unlock()
		return
	}
	w.fireLocked(info)
	w.writeStats()
	w.lock.Unlock()

}

// Hire hires a healthy worker and marks the worker to add a job.
func (w *Workshop) Hire() (Worker, error) {
	w.lock.Lock()
	info, err := w.hireLocked()
	w.writeStats()
	if err != nil {
		w.lock.Unlock()
		return nil, err
	}
	w.lock.Unlock()
	return info.worker, nil
}

// Stats returns the current workshop stats.
func (w *Workshop) Stats() WorkshopStats {
	return w.statsReader.Load().(WorkshopStats)
}

func (w *Workshop) fireLocked(info *workerInfo) {
	w.stats.fireOne()
	if !info.worker.Health() {
		delete(w.infos, info.worker)
		w.stats.Worker--
		return
	}
	info.free()
	jobNum := info.jobNum
	if jobNum == 0 {
		info.idleExpire = coarsetime.CeilingTimeNow().Add(w.maxIdleDuration)
		w.stats.Idle++
	}
	if jobNum >= w.mostFree.jobNum {
		return
	}
	w.mostFree = info
}

func (w *Workshop) hireLocked() (*workerInfo, error) {
	var info *workerInfo
GET:
	info = w.mostFree
	if len(w.infos) >= w.maxQuota || (info != nil && info.jobNum == 0) {
		if info.jobNum == 0 {
			w.stats.Idle--
			if coarsetime.FloorTimeNow().After(info.idleExpire) {
				delete(w.infos, info.worker)
				info.worker.Close()
				w.stats.Worker--
				w.updateFreeLocked()
				goto GET
			}
		}
		w.updateFreeLocked()

	} else {
		var err error
		info, err = w.addFn()
		if err != nil {
			return nil, err
		}
		w.mostFree = info
	}

	if !info.worker.Health() {
		delete(w.infos, info.worker)
		w.stats.Worker--
		if info.jobNum == 0 {
			w.stats.Idle--
		}
		return w.hireLocked()
	}

	info.use()
	w.stats.hireOne()
	return info, nil
}

func (w *Workshop) updateFreeLocked() {
	if len(w.infos) == 0 {
		w.mostFree = nil
		return
	}
	var mostFree *workerInfo
	for _, info := range w.infos {
		if mostFree != nil && info.jobNum >= mostFree.jobNum {
			continue
		}
		mostFree = info
	}
	w.mostFree = mostFree
}

func (w *Workshop) gc() {
	for {
		select {
		case <-w.closeCh:
			return
		default:
			time.Sleep(w.maxIdleDuration)
			w.lock.Lock()
			w.refreshLocked()
			w.lock.Unlock()
		}
	}
}

// Remove the expired or unhealthy idle workers.
func (w *Workshop) refreshLocked() {
	var max, min int32
	var tmp int32
	min = math.MaxInt32
	var shouldUpdate bool
	for _, info := range w.infos {
		if info.jobNum == 0 &&
			(!info.worker.Health() || coarsetime.FloorTimeNow().After(info.idleExpire)) {
			delete(w.infos, info.worker)
			info.worker.Close()
			w.stats.Worker--
			w.stats.Idle--
			shouldUpdate = true
			continue
		}
		tmp = info.jobNum
		if tmp > max {
			max = tmp
		}
		if tmp < min {
			min = tmp
		}
	}
	if shouldUpdate {
		w.updateFreeLocked()
	}
	if min == math.MaxInt32 {
		min = 0
	}
	w.stats.LeastUsed = min
	w.stats.MostUsed = max
	w.writeStats()
}

func (info *workerInfo) use() {
	info.jobNum++
	info.wg.Add(1)
}

func (info *workerInfo) free() {
	info.jobNum--
	info.wg.Add(-1)
}

func (w *Workshop) writeStats() {
	w.statsReader.Store(*w.stats)
}

func (stats *WorkshopStats) hireOne() {
	stats.Hire++
	stats.Doing = int32(stats.Hire - stats.Fire)
}

func (stats *WorkshopStats) fireOne() {
	stats.Fire++
	stats.Doing = int32(stats.Hire - stats.Fire)
}
