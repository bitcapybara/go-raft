package core

import "encoding/gob"

type RaftStatePersister interface {

	SaveRaftState(RaftState) error

	LoadRaftState() (RaftState, error)
}

type SnapshotPersister interface {

	SaveSnapshot(Snapshot) error

	LoadSnapshot() (Snapshot, error)
}

type Persister interface {

	RaftStatePersister

	SnapshotPersister
}

type RaftState struct {
	Term     int
	VotedFor NodeId
	Entries  []Entry
}

type Snapshot struct {
	LastIndex int
	LastTerm  int
	Data     []byte
}

// 持久化器的默认实现，保存在文件中
type DefaultPersister struct {
	FilePath string
}

func NewPersister(fsm Fsm) *DefaultPersister {
	gob.Register(fsm)
	dp := new(DefaultPersister)
	dp.FilePath = "./persist.store"
	return dp
}

func (d *DefaultPersister) SaveRaftState(state RaftState) error {
	panic("implement me")
}

func (d *DefaultPersister) LoadRaftState() (RaftState, error) {
	panic("implement me")
}

func (d *DefaultPersister) SaveSnapshot(snapshot Snapshot) error {
	panic("implement me")
}

func (d *DefaultPersister) LoadSnapshot() (Snapshot, error) {
	panic("implement me")
}
