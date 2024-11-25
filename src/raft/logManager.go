package raft

import (
	"6.5840/labgob"
	"bytes"
	"sync"
)

// LogEntry represents a single log entry in Raft.
type LogEntry struct {
	Term    int         // 任期号
	Command interface{} // 客户端命令
}

// LogManager handles log entries and related operations.
type LogManager struct {
	mu               sync.Mutex
	logs             []LogEntry // 日志条目数组
	lastTrimmedIndex int
	lastTrimmedTerm  int
	// trim即修剪, 此时 即说包含在 快照后的最后一个日志
	// 有什么意义? 意义是 快照包含的 日志条目 已经不在内存中了, 但是又需要在投票的时候 使用到最后一个
	// 日志的 term和index , 所以需要 两个变量保护这个值 , 维持这个值

	persister *Persister // 持久化管理器
}

// NewLogManager creates a new LogManager.
func NewLogManager(persister *Persister) *LogManager {
	lm := &LogManager{
		logs:      make([]LogEntry, 1), // 初始包含一个空日志项，用于索引偏移
		persister: persister,
	}
	lm.Restore(persister.ReadRaftState()) // 从持久化状态恢复日志和快照
	return lm
}

// AppendEntry appends a new log entry to the log.
func (lm *LogManager) AppendEntry(term int, command interface{}) {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	entry := LogEntry{Term: term, Command: command}
	lm.logs = append(lm.logs, entry)
	lm.persist()
}

// GetEntry returns the log entry at a specific index.
func (lm *LogManager) GetEntry(index int) LogEntry {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if index <= lm.lastTrimmedIndex || index >= lm.LastIndex() {
		return LogEntry{}
	}
	return lm.logs[index-lm.lastTrimmedIndex]
	// 这里注意逻辑长度 , 和实际长度的区别, 实际长度是当前logs中有的
	//
}

func (lm *LogManager) LastEntry() *LogEntry {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	// 如果日志数组为空，说明没有日志条目
	if len(lm.logs) == 0 {
		return nil
	}

	// 返回日志数组中的最后一个条目
	return &lm.logs[len(lm.logs)-1]
}

func (rf *Raft) findLastLogIndexOfTerm(term int) int {
	// 从日志数组的末尾开始查找，这样可以快速找到最后一个指定 Term 的索引
	for i := len(rf.lm.logs) - 1; i >= 0; i-- {
		if rf.lm.logs[i].Term == term {
			return i // 返回找到的索引
		}
	}
	return -1 // 如果没有找到指定的 Term，返回 -1
}

func (rf *Raft) findFirstLogIndexOfTerm(term int) int {
	// 从日志数组的开头开始查找，寻找第一个指定 Term 的索引
	for i := 0; i < len(rf.lm.logs); i++ {
		if rf.lm.logs[i].Term == term {
			return i // 返回找到的第一个索引
		}
	}
	return -1 // 如果没有找到指定的 Term，返回 -1
}

// 覆盖冲突的日志, 从leader, 因为以leader为准
// 注意entries是一些存放 指向log 的指针哦
func (lm *LogManager) appendFrom(from int, entries []*LogEntry) {
	trimmedIndex := lm.lastTrimmedIndex
	startIndex := from - trimmedIndex - 1

	// 如果需要覆盖日志
	if startIndex < len(lm.logs) {
		lm.logs = lm.logs[:startIndex]
	}

	// 追加新日志条目（解引用指针）
	for _, entry := range entries {
		lm.logs = append(lm.logs, *entry) // 追加解引用后的日志条目
	}
}

func (lm *LogManager) getEntriesFrom(nextIndex int) []*LogEntry {
	// 计算实际的数组起始索引
	arrayIndex := nextIndex - lm.lastTrimmedIndex - 1

	// 如果 nextIndex 超出当前日志范围，返回空切片
	if arrayIndex < 0 || arrayIndex >= len(lm.logs) {
		return []*LogEntry{}
	}

	// 创建一个新的切片来存储指向 LogEntry 的指针
	result := make([]*LogEntry, len(lm.logs)-arrayIndex)
	for i := arrayIndex; i < len(lm.logs); i++ {
		result[i-arrayIndex] = &lm.logs[i] // 将每个 LogEntry 的地址存储到 result 中
	}

	return result
}

// todo 逻辑上的第一个索引是 lastTrimmedindex+ 1
func (lm *LogManager) FirstIndex() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if len(lm.logs) == 0 {
		// 如果日志为空，则返回修剪后的下一个索引
		return lm.lastTrimmedIndex + 1
	}

	// 否则返回日志数组中第一个条目的索引
	return lm.lastTrimmedIndex + 1
}

// LastIndex returns the index of the last log entry.
func (lm *LogManager) LastIndex() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.lastTrimmedIndex + len(lm.logs) - 1
	// 返回逻辑长度, last
}

// LastTerm returns the term of the last log entry.
func (lm *LogManager) LastTerm() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	if len(lm.logs) > 0 {
		// 表示  有一些日志在内存中, 还没有被快照裁剪
		return lm.logs[len(lm.logs)-1].Term
	}
	// 否则表示刚裁剪完, logs中一个日志都没有
	return lm.lastTrimmedTerm
}

// Persist saves the current log entries and snapshot metadata to stable storage.
func (lm *LogManager) persist() {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w) // 获取 实验提供的 编码器

	// Encode snapshot metadata
	e.Encode(lm.lastTrimmedIndex)
	e.Encode(lm.lastTrimmedTerm)

	// Encode log entries
	e.Encode(lm.logs)

	raftstate := w.Bytes()
	lm.persister.Save(raftstate, lm.persister.ReadSnapshot())
}

// ApplySnapshot replaces the log with the snapshot up to lastIncludedIndex and lastIncludedTerm.
// 用这个方法来实现?
func (lm *LogManager) ApplySnapshot(snapshot []byte, lastIncludedIndex, lastIncludedTerm int) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.lastTrimmedIndex = lastIncludedIndex
	lm.lastTrimmedTerm = lastIncludedTerm
	lm.logs = nil // 清空日志，只保留快照之后的日志条目

	// 持久化快照元数据
	lm.persist()
	lm.persister.Save(lm.persister.ReadRaftState(), snapshot)
}

// todo , 传来的index是已经确定在快照中的 截止索引 ,此时需要 维护一些变量 ,并且维护logs 数组
func (lm *LogManager) trim(index int) {
	// 确保裁剪的索引是合法的
	if index <= lm.lastTrimmedIndex {
		return // 已经裁剪过了，直接返回
	}

	// 计算裁剪的相对索引, 即要裁剪的 索引的绝对值
	relativeIndex := index - lm.lastTrimmedIndex - 1

	// 裁剪日志, 注意判断是否越界, 越界了表示所有 日志都应该清空

	if relativeIndex >= len(lm.logs) {
		lm.logs = nil // 如果裁剪范围包含所有日志，清空日志数组
	} else {
		lm.logs = lm.logs[relativeIndex:] // 保留从裁剪点之后的日志
	}

	// 更新最后裁剪的索引
	lm.lastTrimmedIndex = index
}

// Restore restores log and snapshot metadata from persisted state.
func (lm *LogManager) Restore(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}

	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)

	var lastIncludedIndex int
	var lastIncludedTerm int
	var logs []LogEntry

	if d.Decode(&lastIncludedIndex) != nil ||
		d.Decode(&lastIncludedTerm) != nil ||
		d.Decode(&logs) != nil {
		// handle error
		return
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.lastTrimmedIndex = lastIncludedIndex
	lm.lastTrimmedTerm = lastIncludedTerm
	lm.logs = logs
}

// TruncateLog removes entries before a given index, typically after taking a snapshot.
func (lm *LogManager) TruncateLog(lastIncludedIndex int, lastIncludedTerm int) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if lastIncludedIndex < lm.lastTrimmedIndex {
		return
	}

	lm.logs = lm.logs[lastIncludedIndex-lm.lastTrimmedIndex:]
	lm.lastTrimmedIndex = lastIncludedIndex
	lm.lastTrimmedTerm = lastIncludedTerm
	lm.persist()
}

// len 方法返回逻辑上的日志长度，即最后一个日志的索引（包含已修剪的部分）。
// trim 表示截止到目前, 日志都被 移转到 快照了
// 而 len 是返回逻辑上的 长度
func (lm *LogManager) len() int {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	return lm.lastTrimmedIndex + len(lm.logs)
}

// 截取范围
func (lm *LogManager) split(start, end int) []LogEntry {
	// 确保 start 和 end 的范围合法
	if start < lm.lastTrimmedIndex+1 || start > end || end > lm.lastTrimmedIndex+1+len(lm.logs) {
		return nil // 返回空切片表示范围非法
	}

	// 根据日志的裁剪偏移量调整范围
	relativeStart := start - lm.lastTrimmedIndex - 1
	relativeEnd := end - lm.lastTrimmedIndex - 1

	// 返回指定范围的日志条目
	return lm.logs[relativeStart:relativeEnd]
}
