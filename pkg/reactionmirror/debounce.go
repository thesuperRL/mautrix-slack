package reactionmirror

import (
	"sync"
	"time"
)

type Debouncer struct {
	delay time.Duration
	mu    sync.Mutex
	tasks map[string]*debounceTask
}

type debounceTask struct {
	timer *time.Timer
	fn    func()
}

func NewDebouncer(delay time.Duration) *Debouncer {
	return &Debouncer{
		delay: delay,
		tasks: make(map[string]*debounceTask),
	}
}

func (d *Debouncer) Schedule(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if task, ok := d.tasks[key]; ok {
		task.timer.Stop()
		task.fn = fn
		task.timer.Reset(d.delay)
		return
	}
	task := &debounceTask{fn: fn}
	task.timer = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		current := d.tasks[key]
		delete(d.tasks, key)
		d.mu.Unlock()
		if current != nil && current.fn != nil {
			current.fn()
		}
	})
	d.tasks[key] = task
}
