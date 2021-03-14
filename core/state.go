package core

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// ==================== RoleState ====================

const (
	Leader    RoleStage = iota // 领导者
	Candidate                  // 候选者
	Follower                   // 追随者
)

// 角色类型
type RoleStage uint8

type RoleState struct {
	roleStage RoleStage  // 节点当前角色
	mu        sync.Mutex // 角色并发访问锁
}

func newRoleState() *RoleState {
	return &RoleState{
		roleStage: Follower,
	}
}

func (st *RoleState) setRoleStage(stage RoleStage) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.roleStage = stage
}

func (st *RoleState) getRoleStage() RoleStage {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.roleStage
}

func (st *RoleState) lock(stage RoleStage) bool {
	st.mu.Lock()
	if st.roleStage != stage {
		st.mu.Unlock()
		return false
	}
	return true
}

func (st *RoleState) unlock() {
	st.mu.Unlock()
}

// ==================== HardState ====================

// 需要持久化存储的状态
type HardState struct {
	term      int                // 当前时刻所处的 term
	votedFor  NodeId             // 当前任期获得选票的 Candidate
	entries   []Entry            // 当前节点保存的日志
	persister RaftStatePersister // 持久化器
	mu        sync.Mutex
}

func NewHardState(persister RaftStatePersister) HardState {
	return HardState{
		term:      1,
		votedFor:  "",
		entries:   make([]Entry, 0),
		persister: persister,
	}
}

func (st *HardState) lastEntryIndex() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	lastLogIndex := len(st.entries) - 1
	if lastLogIndex < 0 {
		return 0
	} else {
		return st.entries[lastLogIndex].Index
	}
}

func (st *HardState) currentTerm() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.term
}

// todo 传入的必须是物理索引
func (st *HardState) logEntryTerm(index int) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.entries)-1 < index {
		return 0
	}
	return st.entries[index].Term
}

func (st *HardState) logLength() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.entries)
}

func (st *HardState) setTerm(term int) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.term >= term {
		return nil
	}
	err := st.persist(term, "", st.entries)
	if err != nil {
		return fmt.Errorf("持久化出错，设置 term 属性值失败。%w", err)
	}
	st.term = term
	st.votedFor = ""
	return nil
}

func (st *HardState) termAddAndVote(delta int, voteTo NodeId) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	newTerm := st.term + delta
	err := st.persist(newTerm, voteTo, st.entries)
	if err != nil {
		return fmt.Errorf("持久化出错，设置 term 属性值失败。%w", err)
	}
	st.term = newTerm
	st.votedFor = voteTo
	return nil
}

func (st *HardState) vote(id NodeId) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.votedFor == id {
		return nil
	}
	err := st.persist(st.term, id, st.entries)
	if err != nil {
		return fmt.Errorf("持久化出错，设置 votedFor 属性值失败。%w", err)
	}
	st.votedFor = id
	return nil
}

func (st *HardState) persist(term int, votedFor NodeId, entries []Entry) error {
	raftState := RaftState{
		Term:     term,
		VotedFor: votedFor,
		Entries:  entries,
	}
	err := st.persister.SaveRaftState(raftState)
	if err != nil {
		return fmt.Errorf("raft 状态持久化失败：%w", err)
	}
	return nil
}

func (st *HardState) appendEntry(entry Entry) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	err := st.persist(st.term, st.votedFor, append(st.entries[:], entry))
	if err != nil {
		return fmt.Errorf("持久化出错，设置 entries 属性值失败。%w", err)
	}
	st.entries = append(st.entries, entry)
	return nil
}

func (st *HardState) logEntry(index int) Entry {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.entries[index]
}

func (st *HardState) voted() NodeId {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.votedFor
}

func (st *HardState) truncateEntries(index int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.entries = st.entries[:index]
}

func (st *HardState) clearEntries() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.entries = make([]Entry, 0)
}

func (st *HardState) logEntries(start, end int) []Entry {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.entries[start:end]
}

// ==================== SoftState ====================

// 保存在内存中的实时状态
type SoftState struct {
	commitIndex int // 已经提交的最大的日志索引，由当前节点维护，初始化为0
	lastApplied int // 应用到状态机的最后一个日志索引
	mu          sync.Mutex
}

func newSoftState() *SoftState {
	return &SoftState{
		commitIndex: 0,
		lastApplied: 0,
	}
}

func (st *SoftState) softCommitIndex() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.commitIndex
}

func (st *SoftState) setCommitIndex(index int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.commitIndex = index
}

func (st *SoftState) setLastApplied(index int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastApplied = index
}

func (st *SoftState) lastAppliedAdd() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastApplied += 1
	return st.lastApplied
}

func (st *SoftState) softLastApplied() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.lastApplied
}

// ==================== PeerState ====================

// 对等节点状态和路由表
type PeerState struct {
	peersMap map[NodeId]NodeAddr // 所有节点
	me       NodeId              // 当前节点在 peersMap 中的索引
	leader   NodeId              // 当前 leader 在 peersMap 中的索引
	mu       sync.Mutex
}

func newPeerState(peers map[NodeId]NodeAddr, me NodeId) *PeerState {
	return &PeerState{
		peersMap: peers,
		me:       me,
		leader:   "",
	}
}

func (st *PeerState) leaderIsMe() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.leader == st.me
}

func (st *PeerState) majority() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.peersMap)/2 + 1
}
func (st *PeerState) peers() map[NodeId]NodeAddr {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.peersMap
}

func (st *PeerState) replacePeers(peers map[NodeId]NodeAddr) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.peersMap = peers
}

func (st *PeerState) replacePeersWithBytes(from []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	// 	获取新节点集
	peers, err := decodePeersMap(from)
	if err != nil {
		return err
	}
	st.peersMap = peers
	return nil
}

func decodePeersMap(from []byte) (map[NodeId]NodeAddr, error) {
	var peers map[NodeId]NodeAddr
	decoder := gob.NewDecoder(bytes.NewBuffer(from))
	err := decoder.Decode(&peers)
	if err != nil {
		return nil, err
	} else {
		return peers, nil
	}
}

func (st *PeerState) peersCnt() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.peersMap)
}

func (st *PeerState) isMe(id NodeId) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return id == st.me
}

func (st *PeerState) myId() NodeId {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.me
}

func (st *PeerState) setLeader(id NodeId) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.leader = id
}

func (st *PeerState) leaderId() NodeId {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.leader
}

func (st *PeerState) getLeader() server {
	st.mu.Lock()
	defer st.mu.Unlock()
	return server{
		id:   st.leader,
		addr: st.peersMap[st.leader],
	}
}

// ==================== LeaderState ====================

type followerReplication struct {
	id         NodeId        // 节点标识
	addr       NodeAddr      // 节点地址
	nextIndex  int           // 下一次要发送给各节点的日志索引。由 Leader 维护，初始值为 Leader 最后一个日志的索引 + 1
	matchIndex int           // 已经复制到各节点的最大的日志索引。由 Leader 维护，初始值为0
	rpcBusy    bool          // 是否正在通信
	mu         sync.Mutex    // 锁
	stepDownCh chan int      // 通知主线程降级
	stopCh     chan struct{} // 接收主线程发来的降级通知
	triggerCh  chan struct{} // 触发复制请求
}

// 节点是 Leader 时，保存在内存中的状态
type LeaderState struct {
	stepDownCh    chan int
	followerState map[NodeId]*followerReplication // todo 配置变更后，需要清空
}

func (st *LeaderState) matchIndex(id NodeId) int {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	return st.followerState[id].matchIndex
}

func (st *LeaderState) setMatchAndNextIndex(id NodeId, matchIndex, nextIndex int) {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	st.followerState[id].matchIndex = matchIndex
	st.followerState[id].nextIndex = nextIndex
}

func (st *LeaderState) nextIndex(id NodeId) int {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	return st.followerState[id].nextIndex
}

func (st *LeaderState) setNextIndex(id NodeId, index int) {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	st.followerState[id].nextIndex = index
}

func (st *LeaderState) setRpcBusy(id NodeId, enable bool) {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	st.followerState[id].rpcBusy = enable
}

func (st *LeaderState) isRpcBusy(id NodeId) bool {
	st.followerState[id].mu.Lock()
	defer st.followerState[id].mu.Unlock()
	return st.followerState[id].rpcBusy
}

func (st *LeaderState) followers() map[NodeId]*followerReplication {
	return st.followerState
}

// ==================== timerState ====================

type timerType uint8

const (
	Election timerType = iota
	Heartbeat
)

type timerState struct {
	timeoutTimer *time.Timer // 超时计时器

	electionMinTimeout int // 最小选举超时时间
	electionMaxTimeout int // 最大选举超时时间
	heartbeatTimeout   int // 心跳间隔时间
}

func newTimerState(config Config) *timerState {
	return &timerState{
		electionMinTimeout: config.ElectionMinTimeout,
		electionMaxTimeout: config.ElectionMaxTimeout,
		heartbeatTimeout:   config.HeartbeatTimeout,
	}
}

// 用于计时器已到期后重置
func (st *timerState) setElectionTimer() {
	if st.timeoutTimer == nil {
		st.timeoutTimer = time.NewTimer(st.electionDuration())
	} else {
		st.timeoutTimer.Stop()
		st.timeoutTimer.Reset(st.electionDuration())
	}
}

func (st *timerState) setHeartbeatTimer() {
	if st.timeoutTimer == nil {
		st.timeoutTimer = time.NewTimer(st.heartbeatDuration())
	} else {
		st.timeoutTimer.Stop()
		st.timeoutTimer.Reset(st.heartbeatDuration())
	}
}

func (st *timerState) electionDuration() time.Duration {
	randTimeout := rand.Intn(st.electionMaxTimeout-st.electionMinTimeout) + st.electionMinTimeout
	return time.Millisecond * time.Duration(randTimeout)
}

func (st *timerState) heartbeatDuration() time.Duration {
	return time.Millisecond * time.Duration(st.heartbeatTimeout)
}

func (st *timerState) tick() <-chan time.Time {
	return st.timeoutTimer.C
}

// ==================== snapshotState ====================

type snapshotState struct {
	snapshot     *Snapshot
	persister    SnapshotPersister
	maxLogLength int
	mu           sync.Mutex
}

func newSnapshotState(config Config) *snapshotState {
	persister := config.SnapshotPersister
	snapshot, err := persister.LoadSnapshot()
	if err != nil {
		log.Fatalln(fmt.Errorf("加载快照失败：%w", err))
	}
	return &snapshotState{
		snapshot:     &snapshot,
		persister:    persister,
		maxLogLength: config.MaxLogLength,
	}
}

func (st *snapshotState) save(snapshot Snapshot) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	err := st.persister.SaveSnapshot(snapshot)
	if err != nil {
		return fmt.Errorf("保存快照失败：%w", err)
	}
	st.snapshot = &snapshot
	return nil
}

func (st *snapshotState) needGenSnapshot(commitIndex int) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	need := commitIndex-st.snapshot.LastIndex >= st.maxLogLength
	return need
}

func (st *snapshotState) lastIndex() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshot.LastIndex
}

func (st *snapshotState) lastTerm() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshot.LastTerm
}

func (st *snapshotState) getSnapshot() *Snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.snapshot
}
