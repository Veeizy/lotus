package core

import (
	"context"
	"sync"
)

// FolderStat 预取进度（/api/net/progress 的数据源）。
type FolderStat struct {
	Total  int `json:"total"`
	Done   int `json:"done"`
	Failed int `json:"failed"`
}

type task struct {
	key       string
	folderKey string
	prio      int
	dead      bool
	ctx       context.Context
	cancel    context.CancelFunc
	run       func(ctx context.Context) (any, error)
	done      chan taskResult
}

// TaskResult 任务结果（导出供 node 层使用）。
type TaskResult = taskResult

type taskResult struct {
	Value any
	Err   error
}

// Queue 下载调度：同键去重、优先级插队、并发上限、按文件夹取消与进度。
// 语义与旧 net.js 的 ensureDownload/pump/cancel 等价（PROTOCOL §5）。
type Queue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	tasks   []*task
	byKey   map[string]*task
	running int
	limit   int

	statsMu sync.Mutex
	stats   map[string]*FolderStat
}

func NewQueue(limit int) *Queue {
	q := &Queue{byKey: map[string]*task{}, limit: clampConcurrency(limit), stats: map[string]*FolderStat{}}
	q.cond = sync.NewCond(&q.mu)
	go q.pump()
	return q
}

func clampConcurrency(n int) int {
	if n < 1 {
		return 1
	}
	if n > 16 {
		return 16
	}
	return n
}

func (q *Queue) SetLimit(n int) {
	q.mu.Lock()
	q.limit = clampConcurrency(n)
	q.mu.Unlock()
	q.cond.Broadcast()
}

// SetStats 创建/覆盖某文件夹的进度（prefetch 用，total 含已命中数）。
func (q *Queue) SetStats(folderKey string, st FolderStat) {
	q.statsMu.Lock()
	q.stats[folderKey] = &st
	q.statsMu.Unlock()
}

// Stats 读取进度；无记录返回全 0。
func (q *Queue) Stats(folderKey string) FolderStat {
	q.statsMu.Lock()
	defer q.statsMu.Unlock()
	if st, ok := q.stats[folderKey]; ok {
		return *st
	}
	return FolderStat{}
}

// AllStats 返回全部进度快照（SSE 推送用）。
func (q *Queue) AllStats() map[string]FolderStat {
	q.statsMu.Lock()
	defer q.statsMu.Unlock()
	out := make(map[string]FolderStat, len(q.stats))
	for k, v := range q.stats {
		out[k] = *v
	}
	return out
}

func (q *Queue) bump(folderKey, field string) {
	q.statsMu.Lock()
	if st, ok := q.stats[folderKey]; ok {
		switch field {
		case "done":
			st.Done++
		case "failed":
			st.Failed++
		}
	}
	q.statsMu.Unlock()
}

// Enqueue 提交任务：同 key 去重（进行中的任务直接复用；priority>0 可把排队任务插队）。
// 返回结果通道。缓存命中等由调用方在提交前判断。
func (q *Queue) Enqueue(key, folderKey string, priority int, run func(ctx context.Context) (any, error)) <-chan taskResult {
	q.mu.Lock()
	if exist, ok := q.byKey[key]; ok {
		if priority > 0 && exist.prio == 0 && !exist.dead {
			exist.prio = priority
		}
		q.mu.Unlock()
		return exist.done
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &task{key: key, folderKey: folderKey, prio: priority, ctx: ctx, cancel: cancel,
		run: run, done: make(chan taskResult, 1)}
	q.byKey[key] = t
	q.tasks = append(q.tasks, t)
	q.mu.Unlock()
	q.cond.Broadcast()
	return t.done
}

// Cancel 取消某文件夹的全部任务：删除进度（清零）、剔除排队任务、尽力取消在途任务。
// 在途任务若已完整接收则照常落地缓存（run 内部在完整校验后进入落地阶段，不再响应取消）。
func (q *Queue) Cancel(folderKey string) {
	q.statsMu.Lock()
	delete(q.stats, folderKey)
	q.statsMu.Unlock()

	q.mu.Lock()
	var keep []*task
	var cancels []context.CancelFunc
	for _, t := range q.tasks {
		if t.folderKey == folderKey {
			t.dead = true
			delete(q.byKey, t.key)
			continue
		}
		keep = append(keep, t)
	}
	q.tasks = keep
	for _, t := range q.byKey {
		if t.folderKey == folderKey {
			cancels = append(cancels, t.cancel)
		}
	}
	q.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	q.cond.Broadcast()
}

// Wait 阻塞等待任务结果。
func Wait(ch <-chan taskResult) (any, error) {
	r := <-ch
	return r.Value, r.Err
}

func (q *Queue) pump() {
	for {
		q.mu.Lock()
		for q.running >= q.limit || len(q.tasks) == 0 {
			q.cond.Wait()
		}
		// 优先级插队：找第一个 prio>0 的任务，否则取队首（与旧 pump 一致）
		idx := 0
		for i, t := range q.tasks {
			if t.prio > 0 {
				idx = i
				break
			}
		}
		t := q.tasks[idx]
		q.tasks = append(q.tasks[:idx], q.tasks[idx+1:]...)
		if t.dead {
			delete(q.byKey, t.key)
			t.done <- taskResult{Err: context.Canceled}
			q.mu.Unlock()
			continue
		}
		q.running++
		q.mu.Unlock()

		go func(t *task) {
			val, err := t.run(t.ctx)
			if err != nil {
				q.bump(t.folderKey, "failed")
			} else {
				q.bump(t.folderKey, "done")
			}
			t.done <- taskResult{Value: val, Err: err}
			q.mu.Lock()
			q.running--
			if cur, ok := q.byKey[t.key]; ok && cur == t {
				delete(q.byKey, t.key)
			}
			q.mu.Unlock()
			q.cond.Broadcast()
		}(t)
	}
}
