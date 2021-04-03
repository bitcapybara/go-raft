package raft

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"time"
)

type finishMsgType uint8

const (
	Error finishMsgType = iota
	RpcFailed
	Degrade
	Success
)

type finishMsg struct {
	msgType finishMsgType
	term    int
	id      NodeId
}

// 配置参数
type Config struct {
	Fsm                Fsm
	RaftStatePersister RaftStatePersister
	SnapshotPersister  SnapshotPersister
	Transport          Transport
	Logger             Logger
	Peers              map[NodeId]NodeAddr
	Me                 NodeId
	Role               RoleStage
	ElectionMinTimeout int
	ElectionMaxTimeout int
	HeartbeatTimeout   int
	MaxLogLength       int
}

// 客户端状态机接口
type Fsm interface {
	// 参数实际上是 Entry 的 Data 字段
	// 返回值是应用状态机后的结果
	Apply([]byte) error

	// 生成快照二进制数据
	Serialize() ([]byte, error)
}

type raft struct {
	fsm           Fsm            // 客户端状态机
	transport     Transport      // 发送请求的接口
	logger        Logger         // 日志打印
	roleState     *RoleState     // 当前节点的角色
	hardState     *HardState     // 需要持久化存储的状态
	softState     *SoftState     // 保存在内存中的实时状态
	peerState     *PeerState     // 对等节点状态和路由表
	leaderState   *LeaderState   // 节点是 Leader 时，保存在内存中的状态
	timerState    *timerState    // 计时器状态
	snapshotState *snapshotState // 快照状态

	rpcCh  chan rpc      // 主线程接收 rpc 消息
	exitCh chan struct{} // 当前节点离开节点，退出程序
}

func newRaft(config Config) *raft {
	if config.ElectionMinTimeout > config.ElectionMaxTimeout {
		panic("ElectionMinTimeout 不能大于 ElectionMaxTimeout！")
	}
	raftPst := config.RaftStatePersister

	var raftState RaftState
	if raftPst != nil {
		rfState, err := raftPst.LoadRaftState()
		if err != nil {
			panic(fmt.Sprintf("持久化器加载 RaftState 失败：%s\n", err))
		} else {
			raftState = rfState
		}
	} else {
		panic("缺失 RaftStatePersister!")
	}
	hardState := raftState.toHardState(raftPst)

	return &raft{
		fsm:           config.Fsm,
		transport:     config.Transport,
		logger:        config.Logger,
		roleState:     newRoleState(config.Role),
		hardState:     &hardState,
		softState:     newSoftState(),
		peerState:     newPeerState(config.Peers, config.Me),
		leaderState:   newLeaderState(config.Peers),
		timerState:    newTimerState(config),
		snapshotState: newSnapshotState(config),
		rpcCh:         make(chan rpc),
		exitCh:        make(chan struct{}),
	}
}

func (rf *raft) raftRun(rpcCh chan rpc) {
	rf.rpcCh = rpcCh
	go func() {
		for {
			select {
			case <-rf.exitCh:
				return
			default:
			}
			switch rf.roleState.getRoleStage() {
			case Leader:
				rf.logger.Trace("开启runLeader()循环")
				rf.runLeader()
			case Candidate:
				rf.logger.Trace("开启runCandidate()循环")
				rf.runCandidate()
			case Follower:
				rf.logger.Trace("开启runFollower()循环")
				rf.runFollower()
			case Learner:
				rf.logger.Trace("开启runLearner()循环")
				rf.runLearner()
			}
		}
	}()
}

func (rf *raft) runLeader() {
	rf.logger.Trace("进入 runLeader()")
	// 初始化心跳定时器
	rf.timerState.setHeartbeatTimer()
	rf.logger.Trace("初始化心跳定时器成功")

	// 开启日志复制循环
	rf.runReplication()
	rf.logger.Trace("开启日志复制循环")

	// 节点退出 Leader 状态，收尾工作
	defer func() {
		for _, st := range rf.leaderState.replications {
			close(st.stopCh)
		}
		rf.logger.Trace("退出 runLeader()，关闭各个 replication 的 stopCh")
	}()

	for rf.roleState.getRoleStage() == Leader {
		select {
		case msg := <-rf.rpcCh:
			if transfereeId, busy := rf.leaderState.isTransferBusy(); busy {
				// 如果正在进行领导权转移
				rf.logger.Trace("节点正在进行领导权转移，请求驳回！")
				msg.res <- rpcReply{err: fmt.Errorf("正在进行领导权转移，请求驳回！")}
				rf.checkTransfer(transfereeId)
			} else {
				switch msg.rpcType {
				case AppendEntryRpc:
					rf.logger.Trace("接收到 AppendEntryRpc 请求")
					rf.handleCommand(msg)
				case RequestVoteRpc:
					rf.logger.Trace("接收到 RequestVoteRpc 请求")
					rf.handleVoteReq(msg)
				case ApplyCommandRpc:
					rf.logger.Trace("接收到 ApplyCommandRpc 请求")
					rf.handleClientCmd(msg)
				case ChangeConfigRpc:
					rf.logger.Trace("接收到 ChangeConfigRpc 请求")
					rf.handleConfiguration(msg)
				case TransferLeadershipRpc:
					rf.logger.Trace("接收到 TransferLeadershipRpc 请求")
					rf.handleTransfer(msg)
				case AddNewNodeRpc:
					rf.logger.Trace("接收到 AddNewNodeRpc 请求")
					rf.handleNewNode(msg)
				}
			}
		case <-rf.timerState.tick():
			rf.logger.Trace("心跳计时器到期，开始发送心跳")
			stopCh := make(chan struct{})
			finishCh := rf.heartbeat(stopCh)
			successCnt := 1
			count := 1
			end := false
			for !end {
				select {
				case <-time.After(rf.timerState.heartbeatDuration()):
					rf.logger.Trace("操作超时退出")
					end = true
				case msg := <-finishCh:
					if msg.msgType == Degrade && rf.becomeFollower(msg.term) {
						rf.logger.Trace("降级为 Follower")
						end = true
						break
					}
					if msg.msgType == Success {
						rf.logger.Trace("成功获取到一个心跳结果")
						successCnt += 1
					}
					if successCnt >= rf.peerState.majority() {
						rf.logger.Trace("心跳已成功发送给多数节点")
						end = true
						break
					}
					count += 1
					if count >= rf.peerState.peersCnt() {
						rf.logger.Trace("已接收所有响应，成功节点数未达到多数")
						end = true
						break
					}
				}
			}
			close(stopCh)
		case id := <-rf.leaderState.done:
			if transfereeId, busy := rf.leaderState.isTransferBusy(); busy && transfereeId == id {
				rf.logger.Trace("领导权转移的目标节点日志复制结束，开始领导权转移")
				rf.checkTransfer(transfereeId)
				rf.logger.Trace("领导权转移结束")
			}
		case msg := <-rf.leaderState.stepDownCh:
			// 接收到降级消息
			rf.logger.Trace("接收到降级消息")
			if rf.becomeFollower(msg) {
				rf.logger.Trace("Leader降级成功")
				return
			}
		}
	}
}

func (rf *raft) runCandidate() {
	// 初始化选举计时器
	rf.timerState.setElectionTimer()
	rf.logger.Trace("初始化选举计时器成功")
	// 开始选举
	stopCh := make(chan struct{})
	defer close(stopCh)
	rf.logger.Trace("开始选举")
	finishCh := rf.election(stopCh)

	successCnt := 1
	for rf.roleState.getRoleStage() == Candidate {
		select {
		case <-rf.timerState.tick():
			// 开启下一轮选举
			rf.logger.Trace("选举计时器到期，开始下一轮选举")
			return
		case msg := <-rf.rpcCh:
			switch msg.rpcType {
			case AppendEntryRpc:
				rf.logger.Trace("接收到 AppendEntryRpc 请求")
				rf.handleCommand(msg)
			case RequestVoteRpc:
				rf.logger.Trace("接收到 RequestVoteRpc 请求")
				rf.handleVoteReq(msg)
			}
		case msg := <-finishCh:
			// 降级
			if msg.msgType == Error {
				break
			}
			if msg.msgType == Degrade && rf.becomeFollower(msg.term) {
				rf.logger.Trace("降级为 Follower")
				return
			}
			if msg.msgType == Success {
				successCnt += 1
			}
			// 升级
			if successCnt >= rf.peerState.majority() {
				rf.logger.Trace("获取到多数节点投票")
				if rf.becomeLeader() {
					rf.logger.Trace("升级为 Leader")
				}
				return
			}
		}
	}
}

func (rf *raft) runFollower() {
	// 初始化选举计时器
	rf.timerState.setElectionTimer()
	rf.logger.Trace("初始化选举计时器成功")
	for rf.roleState.getRoleStage() == Follower {
		select {
		case <-rf.timerState.tick():
			// 成为候选者
			rf.logger.Trace("选举计时器到期，开启新一轮选举")
			rf.becomeCandidate()
			return
		case msg := <-rf.rpcCh:
			switch msg.rpcType {
			case AppendEntryRpc:
				rf.logger.Trace("接收到 AppendEntryRpc 请求")
				rf.handleCommand(msg)
			case RequestVoteRpc:
				rf.logger.Trace("接收到 RequestVoteRpc 请求")
				rf.handleVoteReq(msg)
			case InstallSnapshotRpc:
				rf.logger.Trace("接收到 InstallSnapshotRpc 请求")
				rf.handleSnapshot(msg)
			}
		}
	}
}

func (rf *raft) runLearner() {
	for rf.roleState.getRoleStage() == Learner {
		select {
		case msg := <-rf.rpcCh:
			switch msg.rpcType {
			case AppendEntryRpc:
				rf.logger.Trace("接收到 AppendEntryRpc 请求")
				rf.handleCommand(msg)
			}
		}
	}
}

// ==================== logic process ====================

func (rf *raft) heartbeat(stopCh chan struct{}) chan finishMsg {

	// 重置心跳计时器
	rf.timerState.setHeartbeatTimer()
	rf.logger.Trace("重置心跳计时器成功")

	finishCh := make(chan finishMsg)

	for id := range rf.peerState.peers() {
		if rf.peerState.isMe(id) || rf.leaderState.isRpcBusy(id) {
			rf.logger.Trace(fmt.Sprintf("自身和忙节点，不发送心跳。Id=%s", id))
			continue
		}
		rf.logger.Trace(fmt.Sprintf("给 Id=%s 的节点发送心跳", id))
		go rf.replicationTo(id, finishCh, stopCh, EntryHeartbeat)
	}

	return finishCh
}

// Candidate / Follower 开启新一轮选举
func (rf *raft) election(stopCh chan struct{}) <-chan finishMsg {
	// pre-vote
	preVoteFinishCh := rf.sendRequestVote(stopCh)

	if !rf.waitRpcResult(preVoteFinishCh) {
		rf.logger.Trace("preVote 失败，退出选举")
		go func() {preVoteFinishCh <- finishMsg{msgType: Error}}()
		return preVoteFinishCh
	}

	// 增加 Term 数
	err := rf.hardState.termAddAndVote(1, rf.peerState.myId())
	if err != nil {
		rf.logger.Error(fmt.Errorf("增加term，设置votedFor失败%w", err).Error())
	}
	rf.logger.Trace(fmt.Sprintf("增加 Term 数，开始发送 RequestVote 请求。Term=%d", rf.hardState.currentTerm()))

	return rf.sendRequestVote(stopCh)
}

func (rf *raft) sendRequestVote(stopCh <-chan struct{}) chan finishMsg {
	// 发送 RV 请求
	finishCh := make(chan finishMsg)

	args := RequestVote{
		Term:        rf.hardState.currentTerm(),
		CandidateId: rf.peerState.myId(),
	}
	for id, addr := range rf.peerState.peers() {
		if rf.peerState.isMe(id) {
			continue
		}

		go func(id NodeId, addr NodeAddr) {

			var msg finishMsg
			defer func() {
				select {
				case <-stopCh:
					rf.logger.Trace("接收到 stopCh 消息")
				default:
					finishCh <- msg
				}
			}()

			res := &RequestVoteReply{}
			rf.logger.Trace(fmt.Sprintf("发送投票请求：%+v", args))
			rpcErr := rf.transport.RequestVote(addr, args, res)

			if rpcErr != nil {
				rf.logger.Error(fmt.Errorf("调用rpc服务失败：%s%w", addr, rpcErr).Error())
				msg = finishMsg{msgType: RpcFailed}
				return
			}

			if res.VoteGranted {
				// 成功获得选票
				rf.logger.Trace(fmt.Sprintf("成功获得来自 Id=%s 的选票", id))
				msg = finishMsg{msgType: Success}
				return
			}

			term := rf.hardState.currentTerm()
			if res.Term > term {
				// 当前任期数落后，降级为 Follower
				rf.logger.Trace(fmt.Sprintf("当前任期数落后，降级为 Follower, Term=%d, resTerm=%d", term, res.Term))
				msg = finishMsg{msgType: Degrade, term: res.Term}
			}
		}(id, addr)
	}

	return finishCh
}

// msgCh 日志复制协程 -> 主协程，通知协程的任务完成
func (rf *raft) waitRpcResult(finishCh <-chan finishMsg) bool {
	count := 1
	successCnt := 1
	end := false
	for !end {
		select {
		case <-time.After(rf.timerState.heartbeatDuration()):
			rf.logger.Trace("操作超时退出")
			end = true
		case msg := <-finishCh:
			if msg.msgType == Degrade && rf.becomeFollower(msg.term) {
				rf.logger.Trace("接收到降级请求并降级成功")
				end = true
				break
			}
			if msg.msgType == Success {
				rf.logger.Trace("接收到成功响应")
				successCnt += 1
			}
			if successCnt >= rf.peerState.majority() {
				rf.logger.Trace("请求已成功发送给多数节点")
				return true
			}
			count += 1
			if count >= rf.peerState.peersCnt() {
				rf.logger.Trace("已接收所有响应，成功节点数未达到多数")
				return false
			}
		}
	}

	return false
}

func (rf *raft) runReplication() {
	for id, addr := range rf.peerState.peers() {
		rf.addReplication(id, addr)
	}
}

func (rf *raft) addReplication(id NodeId, addr NodeAddr) {
	st, ok := rf.leaderState.replications[id]
	if !ok {
		rf.logger.Trace(fmt.Sprintf("生成节点 Id=%s 的 Replication 对象", id))
		st = &Replication{
			id:         id,
			addr:       addr,
			nextIndex:  rf.lastEntryIndex() + 1,
			matchIndex: 0,
			stepDownCh: rf.leaderState.stepDownCh,
			stopCh:     make(chan struct{}),
			triggerCh:  make(chan struct{}),
		}
		rf.leaderState.replications[id] = st
	}
	go func() {
		for {
			select {
			case <-st.stopCh:
				return
			case <-st.triggerCh:
				func() {
					rf.logger.Trace(fmt.Sprintf("Id=%s 开始日志追赶", id))
					// 设置状态
					rf.leaderState.setRpcBusy(st.id, true)
					defer rf.leaderState.setRpcBusy(st.id, false)
					// 复制日志，成功后将节点角色提升为 Follower
					replicate := rf.replicate(st)
					rf.logger.Trace(fmt.Sprintf("日志追赶结束，返回值=%t", replicate))
					if replicate && rf.leaderState.replications[id].role == Learner {
						func() {
							finishCh := make(chan finishMsg)
							stopCh := make(chan struct{})
							defer close(stopCh)
							rf.logger.Trace("日志追赶成功，且目标节点是 Learner 角色，发送 EntryPromote 请求")
							rf.replicationTo(id, finishCh, stopCh, EntryPromote)
							msg := <-finishCh
							if msg.msgType == Success {
								rf.leaderState.roleUpgrade(st.id)
								rf.peerState.addPeer(st.id, st.addr)
								rf.logger.Trace("目标节点升级为 Follower 成功")
							}
						}()
					}
				}()
			}
		}
	}()
}

// Follower 和 Candidate 接收到来自 Leader 的 AppendEntries 调用
func (rf *raft) handleCommand(rpcMsg rpc) {

	// 重置选举计时器
	rf.timerState.setElectionTimer()
	rf.logger.Trace("重置选举计时器成功")

	args := rpcMsg.req.(AppendEntry)
	replyRes := AppendEntryReply{}
	var replyErr error
	defer func() {
		rpcMsg.res <- rpcReply{
			res: replyRes,
			err: replyErr,
		}
		rf.logger.Trace("向通道发送返回值成功")
	}()

	// 判断 Term
	rfTerm := rf.hardState.currentTerm()
	if args.Term < rfTerm {
		// 发送请求的 Leader 任期数落后
		rf.logger.Trace("发送请求的 Leader 任期数落后与本节点")
		replyRes.Term = rfTerm
		replyRes.Success = false
		return
	}

	// 任期数落后或相等，如果是候选者，需要降级
	// 后续操作都在 Follower / Learner 角色下完成
	stage := rf.roleState.getRoleStage()
	if args.Term > rfTerm && stage != Follower && stage != Learner {
		rf.logger.Trace("遇到更大的 Term 数，降级为 Follower")
		if !rf.becomeFollower(args.Term) {
			replyErr = fmt.Errorf("节点降级失败")
			return
		}
	}

	// 日志一致性检查
	rf.logger.Trace("开始日志一致性检查")
	prevIndex := args.PrevLogIndex
	if prevIndex > rf.lastEntryIndex() {
		func() {
			defer func() {
				rf.logger.Trace(fmt.Sprintf("返回最后一个日志条目的 Term=%d 及此 Term 的首个条目的索引 index=%d",
					replyRes.ConflictTerm, replyRes.ConflictStartIndex))
				replyRes.Term = rfTerm
				replyRes.Success = false
			}()
			// 当前节点不包含索引为 prevIndex 的日志
			rf.logger.Trace(fmt.Sprintf("当前节点不包含索引为 prevIndex=%d 的日志", prevIndex))
			// 返回最后一个日志条目的 Term 及此 Term 的首个条目的索引
			logLength := rf.hardState.logLength()
			if logLength <= 0 {
				replyRes.ConflictStartIndex = rf.snapshotState.lastIndex()
				replyRes.ConflictTerm = rf.snapshotState.lastTerm()
				rf.logger.Trace("当前节点日志为空")
				return
			}

			if entry, entryErr := rf.logEntry(logLength - 1); entryErr != nil {
				rf.logger.Error(entryErr.Error())
				return
			} else {
				replyRes.ConflictTerm = entry.Term
				replyRes.ConflictStartIndex = rf.lastEntryIndex()
				for i := logLength - 1; i >= 0; i-- {
					if iEntry, iEntryErr := rf.logEntry(i); iEntryErr != nil {
						rf.logger.Error(iEntryErr.Error())
						replyRes.ConflictStartIndex = 0
						break
					} else if iEntry.Term == replyRes.ConflictTerm {
						replyRes.ConflictStartIndex = entry.Index
					} else {
						rf.logger.Trace(fmt.Sprintf("第 %d 日志term %d != conflictTerm", i, iEntry.Term))
						break
					}
				}
			}
		}()
		return
	}
	if prevEntry, prevEntryErr := rf.logEntry(prevIndex); prevEntryErr != nil {
		rf.logger.Error(fmt.Errorf("获取 index=%d 的日志失败！%w", prevIndex, prevEntryErr).Error())
		return
	} else if prevTerm := prevEntry.Term; prevTerm != args.PrevLogTerm {
		func() {
			defer func() {
				rf.logger.Trace(fmt.Sprintf("返回最后一个日志条目的 Term=%d 及此 Term 的首个条目的索引 index=%d",
					replyRes.ConflictTerm, replyRes.ConflictStartIndex))
				replyRes.Term = rfTerm
				replyRes.Success = false
			}()
			// 节点包含索引为 prevIndex 的日志但是 Term 数不同
			rf.logger.Trace(fmt.Sprintf("节点包含索引为 prevIndex=%d 的日志但是 args.PrevLogTerm=%d, PrevLogTerm=%d",
				prevIndex, args.PrevLogTerm, prevTerm))
			// 返回 prevIndex 所在 Term 及此 Term 的首个条目的索引
			replyRes.ConflictTerm = prevTerm
			replyRes.ConflictStartIndex = prevIndex
			for i := prevIndex - 1; i >= 0; i-- {
				if iEntry, iEntryErr := rf.logEntry(i); iEntryErr != nil {
					rf.logger.Error(iEntryErr.Error())
					replyRes.ConflictStartIndex = 0
					break
				} else if iEntry.Term == replyRes.ConflictTerm {
					replyRes.ConflictStartIndex = iEntry.Index
				} else {
					rf.logger.Trace(fmt.Sprintf("第 %d 日志term %d != conflictTerm", i, iEntry.Term))
					break
				}
			}
		}()
		return
	}

	newEntryIndex := prevIndex + 1
	if args.EntryType == EntryReplicate {
		// ========== 接收日志条目 ==========
		rf.logger.Trace("接收到日志条目")
		// 如果当前节点已经有此条目但冲突
		if rf.lastEntryIndex() >= newEntryIndex {
			if entry, entryErr := rf.logEntry(newEntryIndex); entryErr != nil {
				rf.logger.Error(entryErr.Error())
			} else if entry.Term != args.Term {
				truncateErr := rf.truncateAfter(newEntryIndex)
				if truncateErr != nil {
					rf.logger.Error(fmt.Errorf("截断日志失败！%w", truncateErr).Error())
					return
				}
				rf.logger.Trace(fmt.Sprintf("当前节点已经有此条目但冲突，直接覆盖, index=%d, Term=%d, entryTerm=%d",
					newEntryIndex, entry.Term, args.Term))
				// 将新条目添加到日志中
				err := rf.addEntry(args.Entries[0])
				if err != nil {
					rf.logger.Error(fmt.Errorf("日志添加新条目失败！%w", err).Error())
					replyRes.Success = false
				} else {
					replyRes.Success = true
				}
				rf.logger.Trace("成功将新条目添加到日志中")
			} else {
				rf.logger.Trace("当前节点已包含新日志")
			}
		}

		// 添加日志后不提交，下次心跳来了再提交
		return
	}

	if args.EntryType == EntryHeartbeat {
		// ========== 接收心跳 ==========
		rf.logger.Trace("接收到心跳")
		rf.peerState.setLeader(args.LeaderId)
		replyRes.Term = rf.hardState.currentTerm()

		// 更新提交索引
		leaderCommit := args.LeaderCommit
		if leaderCommit > rf.softState.getCommitIndex() {
			var err error
			if leaderCommit >= newEntryIndex {
				rf.softState.setCommitIndex(newEntryIndex)
			} else {
				rf.softState.setCommitIndex(leaderCommit)
			}
			rf.logger.Trace(fmt.Sprintf("成功更新提交索引，commitIndex=%d", rf.softState.getCommitIndex()))
			applyErr := rf.applyFsm()
			if applyErr != nil {
				replyErr = err
				replyRes.Success = false
				rf.logger.Trace("日志应用到状态机失败")
			} else {
				replyRes.Success = true
				rf.logger.Trace("日志成功应用到状态机")
			}
		}

		// 当日志量超过阈值时，生成快照
		rf.logger.Trace("检查是否需要生成快照")
		rf.checkSnapshot()
		replyRes.Success = true
		return
	}

	if args.EntryType == EntryChangeConf {
		rf.logger.Trace("接收到成员变更请求")
		configData := args.Entries[0].Data
		peerErr := rf.peerState.replacePeersWithBytes(configData)
		if peerErr != nil {
			replyErr = peerErr
			replyRes.Success = false
			rf.logger.Trace("新配置应用失败")
		}
		rf.logger.Trace(fmt.Sprintf("新配置应用成功，Peers=%v", rf.peerState.peers()))
		replyRes.Success = true
		return
	}

	if args.EntryType == EntryTimeoutNow {
		rf.logger.Trace("接收到 timeoutNow 请求")
		replyRes.Success = rf.becomeCandidate()
		if replyRes.Success {
			rf.logger.Trace("角色成功变为 Candidate")
		} else {
			rf.logger.Trace("角色变为候选者失败")
		}
	}

	// 已接收到全部日志，从 Learner 角色升级为 Follower
	if rf.roleState.getRoleStage() == Learner && args.EntryType == EntryPromote {
		rf.logger.Trace(fmt.Sprintf("Learner 接收到升级请求，Term=%d", args.Term))
		rf.becomeFollower(args.Term)
		replyRes.Success = true
		rf.logger.Trace("成功升级到Follower")
	}
}

// Follower 和 Candidate 接收到来自 Candidate 的 RequestVote 调用
func (rf *raft) handleVoteReq(rpcMsg rpc) {

	args := rpcMsg.req.(RequestVote)
	replyRes := RequestVoteReply{}
	var replyErr error
	defer func() {
		rpcMsg.res <- rpcReply{
			res: replyRes,
			err: replyErr,
		}
	}()

	rfTerm := rf.hardState.currentTerm()

	if rf.roleState.getRoleStage() == Learner {
		rf.logger.Trace("当前节点是 Learner，不投票")
		replyRes.Term = rfTerm
		replyRes.VoteGranted = false
	}

	argsTerm := args.Term
	if argsTerm < rfTerm {
		// 拉票的候选者任期落后，不投票
		rf.logger.Trace(fmt.Sprintf("拉票的候选者任期落后，不投票。Term=%d, args.Term=%d", rfTerm, argsTerm))
		replyRes.Term = rfTerm
		replyRes.VoteGranted = false
		return
	}

	if argsTerm > rfTerm {
		// 角色降级
		needDegrade := rf.roleState.getRoleStage() != Follower
		if needDegrade && !rf.becomeFollower(argsTerm) {
			replyErr = fmt.Errorf("角色降级失败")
			rf.logger.Trace(replyErr.Error())
			return
		}
		rf.logger.Trace(fmt.Sprintf("角色降级成功，argsTerm=%d, currentTerm=%d", argsTerm, rfTerm))
		if !needDegrade {
			if setTermErr := rf.hardState.setTerm(argsTerm); setTermErr != nil {
				replyErr = fmt.Errorf("设置 Term=%d 值失败：%w", argsTerm, setTermErr)
				rf.logger.Trace(replyErr.Error())
				return
			}
		}
	}

	replyRes.Term = argsTerm
	replyRes.VoteGranted = false
	votedFor := rf.hardState.voted()
	if votedFor == "" || votedFor == args.CandidateId {
		// 当前节点是追随者且没有投过票
		rf.logger.Trace("当前节点是追随者且没有投过票，开始比较日志的新旧程度")
		lastIndex := rf.lastEntryIndex()
		lastTerm := rf.lastEntryTerm()
		// 候选者的日志比当前节点的日志要新，则投票
		// 先比较 Term，Term 相同则比较日志长度
		if args.LastLogTerm > lastTerm || (args.LastLogTerm == lastTerm && args.LastLogIndex >= lastIndex) {
			rf.logger.Trace(fmt.Sprintf("候选者日志较新，args.lastTerm=%d, lastTerm=%d, args.lastIndex=%d, lastIndex=%d",
				args.LastLogTerm, lastTerm, args.LastLogIndex, lastIndex))
			voteErr := rf.hardState.vote(args.CandidateId)
			if voteErr != nil {
				replyErr = fmt.Errorf("更新 votedFor 出错，投票失败：%w", voteErr)
				rf.logger.Error(replyErr.Error())
				replyRes.VoteGranted = false
			} else {
				rf.logger.Trace("成功投出一张选票")
				replyRes.VoteGranted = true
			}
		} else {
			rf.logger.Trace(fmt.Sprintf("候选者日志不够新，不投票，args.lastTerm=%d, lastTerm=%d, args.lastIndex=%d, lastIndex=%d",
				args.LastLogTerm, lastTerm, args.LastLogIndex, lastIndex))
		}
	}

	if replyRes.VoteGranted {
		rf.timerState.setElectionTimer()
		rf.logger.Trace("设置选举计时器成功")
	}
}

// 慢 Follower 接收来自 Leader 的 InstallSnapshot 调用
// 目的是加快日志追赶速度
func (rf *raft) handleSnapshot(rpcMsg rpc) {

	args := rpcMsg.req.(InstallSnapshot)
	replyRes := InstallSnapshotReply{}
	var replyErr error
	defer func() {
		rpcMsg.res <- rpcReply{
			res: replyRes,
			err: replyErr,
		}
	}()

	rfTerm := rf.hardState.currentTerm()
	if args.Term < rfTerm {
		// Leader 的 Term 过期，直接返回
		rf.logger.Trace("发送快照的 Leader 任期落后，直接返回")
		replyRes.Term = rfTerm
		return
	}

	// 持久化
	replyRes.Term = rfTerm
	snapshot := Snapshot{
		LastIndex: args.LastIncludedIndex,
		LastTerm:  args.LastIncludedTerm,
		Data:      args.Data,
	}

	saveErr := rf.snapshotState.save(snapshot)
	if saveErr != nil {
		replyErr = fmt.Errorf("持久化快照失败：%w", saveErr)
		return
	}
	rf.logger.Trace("持久化快照成功！")

	if !args.Done {
		// 若传送没有完成，则继续接收数据
		return
	}

	// 保存快照成功，删除多余日志
	if args.LastIncludedIndex < rf.lastEntryIndex() {
		entry, entryErr := rf.logEntry(args.LastIncludedIndex)
		if entryErr != nil {
			rf.logger.Error(fmt.Errorf("获取 index=%d 的日志失败！%w", args.LastIncludedIndex, entryErr).Error())
			return
		}
		if entry.Term == args.LastIncludedTerm {
			rf.logger.Trace("删除快照之前的旧日志")
			if truncateErr := rf.truncateBefore(args.LastIncludedIndex + 1); truncateErr != nil {
				rf.logger.Error(fmt.Errorf("删除日志失败！%w", truncateErr).Error())
			} else {
				rf.logger.Trace("删除日志成功！")
			}
		}
		return
	}

	rf.logger.Trace("清空日志")
	rf.hardState.clearEntries()
}

// 处理领导权转移请求
func (rf *raft) handleTransfer(rpcMsg rpc) {
	// 先发送一次心跳，刷新计时器，以及
	args := rpcMsg.req.(TransferLeadership)
	timer := time.NewTimer(rf.timerState.minElectionTimeout())
	// 设置定时器和rpc应答通道
	rf.leaderState.setTransferBusy(args.Transferee.Id)
	rf.leaderState.setTransferState(timer, rpcMsg.res)
	rf.logger.Trace("成功设置定时器和rpc应答通道")

	// 查看目标节点日志是否最新
	rf.logger.Trace("查看目标节点日志是否最新")
	rf.checkTransfer(args.Transferee.Id)
}

// 处理客户端请求
func (rf *raft) handleClientCmd(rpcMsg rpc) {

	// 重置心跳计时器
	rf.timerState.setHeartbeatTimer()
	rf.logger.Trace("重置心跳计时器成功")

	args := rpcMsg.req.(ApplyCommand)
	var replyRes ApplyCommandReply
	var replyErr error
	defer func() {
		rpcMsg.res <- rpcReply{
			res: replyRes,
			err: replyErr,
		}
	}()

	if !rf.isLeader() {
		rf.logger.Trace("当前节点不是 Leader，请求驳回")
		replyRes = ApplyCommandReply{
			Status: NotLeader,
			Leader: rf.peerState.getLeader(),
		}
		return
	}

	// Leader 先将日志添加到内存
	rf.logger.Trace("将日志添加到内存")
	addEntryErr := rf.addEntry(Entry{Term: rf.hardState.currentTerm(), Type: EntryReplicate, Data: args.Data})
	if addEntryErr != nil {
		replyErr = fmt.Errorf("Leader 添加客户端日志失败：%w", addEntryErr)
		rf.logger.Trace(replyErr.Error())
		return
	}

	// 给各节点发送日志条目
	finishCh := make(chan finishMsg)
	stopCh := make(chan struct{})
	defer close(stopCh)
	rf.logger.Trace("给各节点发送日志条目")
	for id := range rf.peerState.peers() {
		// 不用给自己发，正在复制日志的不发
		if rf.peerState.isMe(id) || rf.leaderState.isRpcBusy(id) {
			continue
		}
		// 发送日志
		go rf.replicationTo(id, finishCh, stopCh, EntryReplicate)
	}

	// 新日志成功发送到过半 Follower 节点，提交本地的日志
	success := rf.waitRpcResult(finishCh)
	if !success {
		replyErr = fmt.Errorf("rpc 完成，但日志未复制到多数节点")
		rf.logger.Trace(replyErr.Error())
		return
	}

	// 将 commitIndex 设置为新条目的索引
	// 此操作会连带提交 Leader 先前未提交的日志条目并应用到状态季节
	rf.logger.Trace("Leader 更新 commitIndex")
	updateCmtErr := rf.updateLeaderCommit()
	if updateCmtErr != nil {
		replyErr = fmt.Errorf("Leader 更新 commitIndex 失败：%w", updateCmtErr)
		rf.logger.Trace(replyErr.Error())
		return
	}

	// 当日志量超过阈值时，生成快照
	rf.logger.Trace("检查是否需要生成快照")
	rf.checkSnapshot()

	replyRes.Status = OK
}

// 处理成员变更请求
func (rf *raft) handleConfiguration(msg rpc) {
	newConfig := msg.req.(ChangeConfig)
	replyRes := AppendEntryReply{}
	var replyErr error
	defer func() {
		msg.res <- rpcReply{
			res: replyRes,
			err: replyErr,
		}
	}()

	// C(new) 配置
	newPeers := newConfig.Peers
	rf.leaderState.setNewConfig(newPeers)
	oldPeers := rf.peerState.peers()
	rf.leaderState.setOldConfig(oldPeers)
	rf.logger.Trace(fmt.Sprintf("旧配置：%s，新配置%s", oldPeers, newPeers))

	// C(old,new) 配置
	oldNewPeers := make(map[NodeId]NodeAddr)
	for id, addr := range oldPeers {
		oldNewPeers[id] = addr
	}
	for id, addr := range newPeers {
		oldNewPeers[id] = addr
	}
	rf.logger.Trace(fmt.Sprintf("C(old,new)=%s", oldNewPeers))

	// 分发 C(old,new) 配置
	rf.logger.Trace("分发 C(old,new) 配置")
	if oldNewConfigErr := rf.sendOldNewConfig(oldNewPeers); oldNewConfigErr != nil {
		replyErr = oldNewConfigErr
		rf.logger.Trace("C(old,new) 配置分发失败")
		return
	}

	// 分发 C(new) 配置
	rf.logger.Trace("分发 C(new) 配置")
	if newConfigErr := rf.sendNewConfig(newPeers); newConfigErr != nil {
		replyErr = newConfigErr
		rf.logger.Trace("C(new) 配置分发失败")
		return
	}

	// 清理 replications
	peers := rf.peerState.peers()
	// 如果当前节点被移除，退出程序
	if _, ok := peers[rf.peerState.myId()]; !ok {
		rf.logger.Trace("新配置中不包含当前节点，程序退出")
		rf.exitCh <- struct{}{}
		return
	}
	// 查看follower有没有被移除的
	rf.logger.Trace("删除新配置中不包含的 replication")
	followers := rf.leaderState.followers()
	for id, f := range followers {
		if _, ok := peers[id]; !ok {
			f.stopCh <- struct{}{}
			delete(followers, id)
		}
	}
	replyRes.Success = true
}

// 处理添加新节点请求
func (rf *raft) handleNewNode(msg rpc) {
	req := msg.req.(AddNewNode)
	newNode := req.NewNode
	// 开启复制循环
	rf.logger.Trace("新空白节点添加到 replication，并触发复制循环")
	rf.addReplication(newNode.Id, newNode.Addr)
	// 触发复制
	rf.leaderState.followers()[newNode.Id].triggerCh <- struct{}{}
}

func (rf *raft) checkSnapshot() {
	go func() {
		if rf.needGenSnapshot() {
			rf.logger.Trace("达成生成快照的条件")
			data, serializeErr := rf.fsm.Serialize()
			if serializeErr != nil {
				rf.logger.Error(fmt.Errorf("状态机生成快照失败！%w", serializeErr).Error())
			}
			rf.logger.Trace("状态机生成快照成功")
			newSnapshot := Snapshot{
				LastIndex: rf.softState.getLastApplied(),
				LastTerm:  rf.hardState.currentTerm(),
				Data:      data,
			}
			saveErr := rf.snapshotState.save(newSnapshot)
			if saveErr != nil {
				rf.logger.Error(fmt.Errorf("保存快照失败！%w", serializeErr).Error())
			}
			rf.logger.Trace("持久化快照成功")
		}
	}()
}

func (rf *raft) checkTransfer(id NodeId) {
	select {
	case <-rf.leaderState.transfer.timer.C:
		rf.logger.Trace("领导权转移超时")
		rf.leaderState.setTransferBusy(None)
	default:
		if rf.leaderState.isRpcBusy(id) {
			// 若目标节点正在复制日志，则继续等待
			rf.logger.Trace("目标节点正在进行日志复制，继续等待")
			return
		}
		if rf.leaderState.matchIndex(id) == rf.lastEntryIndex() {
			// 目标节点日志已是最新，发送 timeoutNow 消息
			func() {
				var replyRes AppendEntryReply
				var replyErr error
				defer func() {
					rf.leaderState.transfer.reply <- rpcReply{
						res: replyRes,
						err: replyErr,
					}
				}()
				rf.logger.Trace(fmt.Sprintf("目标节点 Id=%s 日志已是最新，发送 timeoutNow 消息", id))
				args := AppendEntry{EntryType: EntryTimeoutNow}
				res := &AppendEntryReply{}
				rpcErr := rf.transport.AppendEntries(rf.peerState.peers()[id], args, res)
				if rpcErr != nil {
					replyErr = fmt.Errorf("rpc 调用失败。%w", rpcErr)
					rf.logger.Trace(replyErr.Error())
					return
				}
				term := rf.hardState.currentTerm()
				if res.Term > term {
					term = res.Term
					replyErr = fmt.Errorf("Term 落后，角色降级")
					rf.logger.Trace(replyErr.Error())
				} else {
					rf.logger.Trace(fmt.Sprintf("领导权转移结果：%t", res.Success))
					if res.Success {
						rf.becomeFollower(term)
						rf.leaderState.setTransferBusy(None)
					}
					replyRes = *res
				}
			}()
		} else {
			// 目标节点不是最新，开始日志复制
			rf.logger.Trace("目标节点不是最新，开始日志复制")
			rf.leaderState.replications[id].triggerCh <- struct{}{}
		}
	}
}

func (rf *raft) sendOldNewConfig(peers map[NodeId]NodeAddr) error {

	oldNewPeersData, enOldNewErr := encodePeersMap(peers)
	if enOldNewErr != nil {
		return fmt.Errorf("序列化peers字典失败！%w", enOldNewErr)
	}

	// C(old,new)配置添加到状态
	addEntryErr := rf.addEntry(Entry{Type: EntryChangeConf, Data: oldNewPeersData})
	if addEntryErr != nil {
		return fmt.Errorf("将配置添加到日志失败！%w", addEntryErr)
	}
	rf.peerState.replacePeers(peers)

	// C(old,new)发送到各个节点
	// 先给旧节点发，再给新节点发
	if rf.waitForConfig(rf.leaderState.getOldConfig()) {
		rf.logger.Trace("配置成功发送到旧节点的多数")
		if rf.waitForConfig(rf.leaderState.getNewConfig()) {
			rf.logger.Trace("配置成功发送到新节点的多数")
			return nil
		} else {
			rf.logger.Trace("配置复制到新配置多数节点失败")
			return fmt.Errorf("配置未复制到新配置多数节点")
		}
	} else {
		rf.logger.Trace("配置复制到旧配置多数节点失败")
		return fmt.Errorf("配置未复制到旧配置多数节点")
	}
}

func (rf *raft) sendNewConfig(peers map[NodeId]NodeAddr) error {

	oldNewPeersData, enOldNewErr := encodePeersMap(peers)
	if enOldNewErr != nil {
		return fmt.Errorf("新配置序列化失败！%w", enOldNewErr)
	}

	// C(old,new)配置添加到状态
	addEntryErr := rf.addEntry(Entry{Type: EntryChangeConf, Data: oldNewPeersData})
	if addEntryErr != nil {
		return fmt.Errorf("将配置添加到日志失败！%w", addEntryErr)
	}
	rf.peerState.replacePeers(peers)
	rf.logger.Trace("替换掉当前节点的 Peers 配置")

	// C(old,new)发送到各个节点
	finishCh := make(chan finishMsg)
	stopCh := make(chan struct{})
	defer close(stopCh)
	rf.logger.Trace("给各节点发送新配置")
	for id := range rf.peerState.peers() {
		// 不用给自己发
		if rf.peerState.isMe(id) {
			continue
		}
		// 发送日志
		rf.logger.Trace(fmt.Sprintf("给 Id=%s 的节点发送配置", id))
		go rf.replicationTo(id, finishCh, stopCh, EntryChangeConf)
	}

	count := 1
	successCnt := 1
	end := false
	for !end {
		select {
		case <-time.After(rf.timerState.heartbeatDuration()):
			return fmt.Errorf("请求超时")
		case msg := <-finishCh:
			if msg.msgType == Degrade {
				rf.logger.Trace("接收到降级请求")
				if rf.becomeFollower(msg.term) {
					rf.logger.Trace("降级成功")
					return fmt.Errorf("降级为 Follower")
				}
			}
			if msg.msgType == Success {
				successCnt += 1
			}
			count += 1
			if successCnt >= rf.peerState.majority() {
				rf.logger.Trace("已发送到大多数节点")
				end = true
				break
			}
			if count >= rf.peerState.peersCnt() {
				return fmt.Errorf("各节点已响应，但成功数不占多数")
			}
		}
	}

	// 提交日志
	rf.logger.Trace("提交新配置日志")
	rf.softState.setCommitIndex(rf.lastEntryIndex())
	return nil
}

func (rf *raft) waitForConfig(peers map[NodeId]NodeAddr) bool {
	finishCh := make(chan finishMsg)
	stopCh := make(chan struct{})
	defer close(stopCh)

	for id := range peers {
		// 不用给自己发
		if rf.peerState.isMe(id) {
			continue
		}
		// 发送日志
		rf.logger.Trace(fmt.Sprintf("给节点 Id=%s 发送最新条目", id))
		go rf.replicationTo(id, finishCh, stopCh, EntryChangeConf)
	}

	count := 1
	successCnt := 1
	end := false
	for !end {
		select {
		case <-time.After(rf.timerState.heartbeatDuration()):
			end = true
			rf.logger.Trace("超时退出")
		case result := <-finishCh:
			if result.msgType == Degrade {
				rf.logger.Trace("接收到降级消息")
				if rf.becomeFollower(result.term) {
					rf.logger.Trace("降级为 Follower")
					return false
				}
				rf.logger.Trace("降级失败")
			}
			if result.msgType == Success {
				rf.logger.Trace("接收到一个成功响应")
				successCnt += 1
			}
			count += 1
			if successCnt >= rf.peerState.majority() {
				rf.logger.Trace("多数节点已成功响应")
				end = true
				break
			}
			if count >= rf.peerState.peersCnt() {
				rf.logger.Trace("接收到所有响应，但成功不占多数")
				return false
			}
		}
	}

	// 提交日志
	rf.logger.Trace("提交日志")
	oldNewIndex := rf.lastEntryIndex()
	rf.softState.setCommitIndex(oldNewIndex)
	return true
}

func encodePeersMap(peers map[NodeId]NodeAddr) ([]byte, error) {
	var data bytes.Buffer
	encoder := gob.NewEncoder(&data)
	enErr := encoder.Encode(peers)
	if enErr != nil {
		return nil, enErr
	}
	return data.Bytes(), nil
}

// Leader 给某个节点发送心跳/日志
func (rf *raft) replicationTo(id NodeId, finishCh chan finishMsg, stopCh chan struct{}, entryType EntryType) {
	var msg finishMsg
	defer func() {
		select {
		case <-stopCh:
		default:
			finishCh <- msg
		}
	}()

	rf.logger.Trace(fmt.Sprintf("给节点 %s 发送 %s 类型的 entry", id, EntryTypeToString(entryType)))

	// 发起 RPC 调用
	addr := rf.peerState.peers()[id]
	prevIndex := rf.leaderState.nextIndex(id) - 1
	var entries []Entry
	if entryType != EntryHeartbeat && entryType != EntryPromote {
		lastEntryIndex := rf.lastEntryIndex()
		entry, err := rf.logEntry(lastEntryIndex)
		if err != nil {
			msg = finishMsg{msgType: Error}
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", lastEntryIndex, err).Error())
			return
		}
		entries = []Entry{entry}
	}
	var prevTerm int
	if prevIndex <= 0 {
		prevTerm = 0
	} else {
		prevEntry, prevEntryErr := rf.logEntry(prevIndex)
		if prevEntryErr != nil {
			msg = finishMsg{msgType: Error}
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", prevIndex, prevEntryErr).Error())
			return
		}
		prevTerm = prevEntry.Term
	}

	args := AppendEntry{
		EntryType:    entryType,
		Term:         rf.hardState.currentTerm(),
		LeaderId:     rf.peerState.myId(),
		PrevLogIndex: prevIndex,
		PrevLogTerm:  prevTerm,
		Entries:      entries,
		LeaderCommit: rf.softState.getCommitIndex(),
	}
	res := &AppendEntryReply{}
	rf.logger.Trace(fmt.Sprintf("发送的内容：%+v", args))
	err := rf.transport.AppendEntries(addr, args, res)

	// 处理 RPC 调用结果
	if err != nil {
		rf.logger.Error(fmt.Errorf("调用rpc服务失败：%s%w\n", addr, err).Error())
		msg = finishMsg{msgType: RpcFailed}
		return
	}

	if res.Success {
		msg = finishMsg{msgType: Success}
		rf.logger.Trace("成功获取到响应")
		return
	}

	if res.Term > rf.hardState.currentTerm() {
		// 当前任期数落后，降级为 Follower
		rf.logger.Trace("任期落后，发送降级通知")
		msg = finishMsg{msgType: Degrade, term: res.Term}
	} else if entryType != EntryChangeConf {
		// Follower 和 Leader 的日志不匹配，进行日志追赶
		rf.logger.Trace("日志进度落后，触发追赶")
		rf.leaderState.replications[id].triggerCh <- struct{}{}
		msg = finishMsg{msgType: Success}
	}
}

// 日志追赶
func (rf *raft) replicate(s *Replication) bool {
	// 向前查找 nextIndex 值
	rf.logger.Trace("向前查找 nextIndex 值")
	if rf.findCorrectNextIndex(s) {
		// 递增更新 matchIndex 值
		rf.logger.Trace("递增更新 matchIndex 值")
		return rf.completeEntries(s)
	}
	rf.logger.Trace("日志追赶失败")
	return false
}

func (rf *raft) findCorrectNextIndex(s *Replication) bool {
	rl := rf.leaderState

	for rl.nextIndex(s.id) > 1 {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		prevIndex := rl.nextIndex(s.id) - 1
		// 找到匹配点之前，发送空日志节省带宽
		var entries []Entry
		if rl.matchIndex(s.id) == prevIndex {
			if entry, entryErr := rf.logEntry(prevIndex); entryErr != nil {
				rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", prevIndex, entryErr).Error())
				return false
			} else {
				entries = []Entry{entry}
			}
		}
		prevEntry, prevEntryErr := rf.logEntry(prevIndex)
		if prevEntryErr != nil {
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", prevIndex, prevEntryErr).Error())
			return false
		}
		args := AppendEntry{
			Term:         rf.hardState.currentTerm(),
			LeaderId:     rf.peerState.myId(),
			PrevLogIndex: prevIndex,
			PrevLogTerm:  prevEntry.Term,
			LeaderCommit: rf.softState.getCommitIndex(),
			Entries:      entries,
		}
		res := &AppendEntryReply{}
		rf.logger.Trace(fmt.Sprintf("给节点 Id=%s 发送日志：%+v", s.id, args))
		err := rf.transport.AppendEntries(s.addr, args, res)

		if err != nil {
			rf.logger.Error(fmt.Errorf("调用rpc服务失败：%s%w\n", s.addr, err).Error())
			return false
		}
		rf.logger.Trace(fmt.Sprintf("接收到应答%+v", res))
		// 如果任期数小，降级为 Follower
		if res.Term > rf.hardState.currentTerm() {
			rf.logger.Trace("当前任期数小，降级为 Follower")
			if rf.becomeFollower(res.Term) {
				rf.logger.Trace("降级成功")
			}
			return false
		}
		if res.Success {
			rf.logger.Trace("日志匹配成功！")
			return true
		}

		conflictStartIndex := res.ConflictStartIndex
		// Follower 日志是空的，则 nextIndex 置为 1
		if conflictStartIndex <= 0 {
			conflictStartIndex = 1
		}
		// conflictStartIndex 处的日志是一致的，则 nextIndex 置为下一个
		if entry, entryErr := rf.logEntry(conflictStartIndex); entryErr != nil {
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", conflictStartIndex, entryErr).Error())
			return false
		} else if entry.Term == res.ConflictTerm {
			conflictStartIndex += 1
		}

		// 向前继续查找 Follower 缺少的第一条日志的索引
		rf.logger.Trace(fmt.Sprintf("设置节点 Id=%s 的 nextIndex 为 %d", s.id, conflictStartIndex))
		rl.setNextIndex(s.id, conflictStartIndex)
	}
	return true
}

func (rf *raft) completeEntries(s *Replication) bool {

	rl := rf.leaderState
	for rl.nextIndex(s.id)-1 < rf.lastEntryIndex() {
		select {
		case <-s.stopCh:
			return false
		default:
		}
		// 缺失的日志太多时，直接发送快照
		snapshot := rf.snapshotState.getSnapshot()
		finishCh := make(chan finishMsg)
		if rl.nextIndex(s.id) <= snapshot.LastIndex {
			rf.logger.Trace(fmt.Sprintf("节点 Id=%s 缺失的日志太多，直接发送快照", s.id))
			rf.snapshotTo(s.addr, snapshot.Data, finishCh, make(chan struct{}))
			msg := <-finishCh
			if msg.msgType != Success {
				if msg.msgType == Degrade {
					rf.logger.Trace("接收到降级通知")
					if rf.becomeFollower(msg.term) {
						rf.logger.Trace("降级为 Follower 成功！")
					}
					return false
				}
			}
			rf.logger.Trace("快照发送成功！")
			rf.leaderState.setMatchAndNextIndex(s.id, snapshot.LastIndex, snapshot.LastIndex+1)
			if snapshot.LastIndex == rf.lastEntryIndex() {
				rf.logger.Trace("快照后面没有新日志，日志追赶结束")
				return true
			}
		}

		prevIndex := rl.nextIndex(s.id) - 1
		prevEntry, prevEntryErr := rf.logEntry(prevIndex)
		if prevEntryErr != nil {
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", prevIndex, prevEntryErr).Error())
			return false
		}
		var entries []Entry
		if entry, entryErr := rf.logEntry(prevIndex); entryErr != nil {
			rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", prevIndex, entryErr).Error())
			return false
		} else {
			entries = []Entry{entry}
		}
		args := AppendEntry{
			Term:         rf.hardState.currentTerm(),
			LeaderId:     rf.peerState.myId(),
			PrevLogIndex: prevIndex,
			PrevLogTerm:  prevEntry.Term,
			LeaderCommit: rf.softState.getCommitIndex(),
			Entries:      entries,
		}
		res := &AppendEntryReply{}
		rf.logger.Trace(fmt.Sprintf("给节点 Id=%s 发送日志：%+v", s.id, args))
		rpcErr := rf.transport.AppendEntries(s.addr, args, res)

		if rpcErr != nil {
			rf.logger.Error(fmt.Errorf("调用rpc服务失败：%s%w\n", s.addr, rpcErr).Error())
			return false
		}
		if res.Term > rf.hardState.currentTerm() {
			rf.logger.Trace("任期数小，开始降级")
			if rf.becomeFollower(res.Term) {
				rf.logger.Trace("降级为 Follower 成功！")
			}
			return false
		}

		// 向后补充
		matchIndex := rl.nextIndex(s.id)
		rf.logger.Trace(fmt.Sprintf("设置节点 Id=%s 的状态：matchIndex=%d, nextIndex=%d", s.id, matchIndex, matchIndex+1))
		rf.leaderState.setMatchAndNextIndex(s.id, matchIndex, matchIndex+1)
	}
	return true
}

func (rf *raft) snapshotTo(addr NodeAddr, data []byte, finishCh chan finishMsg, stopCh chan struct{}) {
	var msg finishMsg
	defer func() {
		select {
		case <-stopCh:
		default:
			finishCh <- msg
		}
	}()
	commitIndex := rf.softState.getCommitIndex()
	entry, entryErr := rf.logEntry(commitIndex)
	if entryErr != nil {
		rf.logger.Error(fmt.Errorf("获取 index=%d 日志失败 %w", commitIndex, entryErr).Error())
		msg = finishMsg{msgType: Error}
		return
	}
	args := InstallSnapshot{
		Term:              rf.hardState.currentTerm(),
		LeaderId:          rf.peerState.myId(),
		LastIncludedIndex: commitIndex,
		LastIncludedTerm:  entry.Term,
		Offset:            0,
		Data:              data,
		Done:              true,
	}
	res := &InstallSnapshotReply{}
	rf.logger.Trace(fmt.Sprintf("向节点 %s 发送快照：%+v", addr, args))
	err := rf.transport.InstallSnapshot(addr, args, res)
	if err != nil {
		rf.logger.Error(fmt.Errorf("调用rpc服务失败：%s%w\n", addr, err).Error())
		msg = finishMsg{msgType: RpcFailed}
		return
	}
	if res.Term > rf.hardState.currentTerm() {
		// 如果任期数小，降级为 Follower
		rf.logger.Trace("任期数小，发送降级通知")
		msg = finishMsg{msgType: Degrade, term: res.Term}
		return
	}
	msg = finishMsg{msgType: Success}
	rf.logger.Trace("发送快照成功！")
}

// 当前节点是不是 Leader
func (rf *raft) isLeader() bool {
	roleStage := rf.roleState.getRoleStage()
	leaderIsMe := rf.peerState.leaderIsMe()
	return roleStage == Leader && leaderIsMe
}

func (rf *raft) becomeLeader() bool {
	rf.setRoleStage(Leader)
	rf.peerState.setLeader(rf.peerState.myId())

	// 给各个节点发送心跳，建立权柄
	finishCh := make(chan finishMsg)
	stopCh := make(chan struct{})
	rf.logger.Trace("给各个节点发送心跳，建立权柄")
	for id := range rf.peerState.peers() {
		if rf.peerState.isMe(id) {
			continue
		}
		rf.logger.Trace(fmt.Sprintf("给 Id=%s 发送心跳", id))
		go rf.replicationTo(id, finishCh, stopCh, EntryHeartbeat)
	}
	return true
}

func (rf *raft) becomeCandidate() bool {
	// 角色置为候选者
	rf.setRoleStage(Candidate)
	return true
}

// 降级为 Follower
func (rf *raft) becomeFollower(term int) bool {

	rf.logger.Trace("设置节点 Term 值")
	err := rf.hardState.setTerm(term)
	if err != nil {
		rf.logger.Error(fmt.Errorf("term 值设置失败，降级失败%w", err).Error())
		return false
	}
	rf.setRoleStage(Follower)
	return true
}

func (rf *raft) setRoleStage(stage RoleStage) {
	rf.roleState.setRoleStage(stage)
	rf.logger.Trace(fmt.Sprintf("角色设置为 %s", RoleToString(stage)))
	if stage == Leader {
		rf.peerState.setLeader(rf.peerState.myId())
	}
}

// 添加新日志
func (rf *raft) addEntry(entry Entry) error {
	index := 1
	lastLogIndex := rf.lastEntryIndex()
	lastSnapshotIndex := rf.snapshotState.lastIndex()
	if lastLogIndex <= 0 {
		if lastSnapshotIndex <= 0 {
			entry.Index = index
		} else {
			entry.Index = lastSnapshotIndex
		}
	} else {
		entry.Index = lastLogIndex
	}
	rf.logger.Trace(fmt.Sprintf("日志条目索引 index=%d", entry.Index))
	return rf.hardState.appendEntry(entry)
}

// 把日志应用到状态机
func (rf *raft) applyFsm() error {
	commitIndex := rf.softState.getCommitIndex()
	lastApplied := rf.softState.getLastApplied()

	for commitIndex > lastApplied {
		if entry, entryErr := rf.logEntry(lastApplied + 1); entryErr != nil {
			err := fmt.Errorf("获取 index=%d 日志失败 %w", lastApplied+1, entryErr)
			rf.logger.Error(err.Error())
			return err
		} else {
			err := rf.fsm.Apply(entry.Data)
			if err != nil {
				return fmt.Errorf("应用状态机失败：%w", err)
			}
			lastApplied = rf.softState.lastAppliedAdd()
		}
	}

	return nil
}

// 更新 Leader 的提交索引
func (rf *raft) updateLeaderCommit() error {
	indexCnt := make(map[int]int)
	peers := rf.peerState.peers()
	//
	for id := range peers {
		indexCnt[rf.leaderState.matchIndex(id)] = 1
	}

	// 计算出多少个节点有相同的 matchIndex 值
	for index := range indexCnt {
		for index2, cnt2 := range indexCnt {
			if index > index2 {
				indexCnt[index2] = cnt2 + 1
			}
		}
	}

	// 找出超过半数的 matchIndex 值
	maxMajorityMatch := 0
	for index, cnt := range indexCnt {
		if cnt >= rf.peerState.majority() && index > maxMajorityMatch {
			maxMajorityMatch = index
		}
	}

	if rf.softState.getCommitIndex() < maxMajorityMatch {
		rf.softState.setCommitIndex(maxMajorityMatch)
		return rf.applyFsm()
	}

	return nil
}

func (rf *raft) needGenSnapshot() bool {
	return rf.softState.getCommitIndex()-rf.snapshotState.lastIndex() >= rf.snapshotState.logThreshold()
}

func (rf *raft) lastEntryIndex() (index int) {
	if length := rf.hardState.logLength(); length > 0 {
		entry, _ := rf.hardState.logEntry(length - 1)
		index = entry.Index
	} else if snapshot := rf.snapshotState.getSnapshot(); snapshot != nil {
		index = snapshot.LastIndex
	} else {
		index = 0
	}
	return
}

func (rf *raft) lastEntryTerm() (term int) {
	if lastEntryIndex := rf.lastEntryIndex(); lastEntryIndex > 0 {
		entry, _ := rf.hardState.logEntry(lastEntryIndex)
		term = entry.Term
	} else {
		term = 0
	}
	return
}

// 索引必须大于快照的索引
func (rf *raft) logEntry(index int) (entry Entry, err error) {
	if snapshot := rf.snapshotState.getSnapshot(); snapshot != nil {
		if index <= snapshot.LastIndex {
			err = errors.New(fmt.Sprintf("索引 %d 小于快照索引 %d，不合法操作", index, snapshot.LastIndex))
		} else {
			if iEntry, iEntryErr := rf.hardState.logEntry(index - snapshot.LastIndex - 1); iEntryErr != nil {
				err = fmt.Errorf(iEntryErr.Error())
			} else {
				entry = iEntry
			}
		}
	} else {
		if iEntry, iEntryErr := rf.hardState.logEntry(index); iEntryErr != nil {
			err = fmt.Errorf(iEntryErr.Error())
		} else {
			entry = iEntry
		}
	}
	return
}

// 将当前索引及之后的日志删除
func (rf *raft) truncateAfter(index int) (err error) {
	if snapshot := rf.snapshotState.getSnapshot(); snapshot != nil {
		if index <= snapshot.LastIndex {
			err = errors.New(fmt.Sprintf("索引 %d 小于快照索引 %d，不合法操作", index, snapshot.LastIndex))
		} else {
			rf.hardState.truncateAfter(index - snapshot.LastIndex - 1)
		}
	} else {
		rf.hardState.truncateAfter(index)
	}
	return
}

// 将当前索引之前的日志删除
func (rf *raft) truncateBefore(index int) (err error) {
	if snapshot := rf.snapshotState.getSnapshot(); snapshot != nil {
		if index <= snapshot.LastIndex {
			err = errors.New(fmt.Sprintf("索引 %d 小于快照索引 %d，不合法操作", index, snapshot.LastIndex))
		} else {
			rf.hardState.truncateBefore(index - snapshot.LastIndex - 1)
		}
	} else {
		rf.hardState.truncateBefore(index)
	}
	return
}
