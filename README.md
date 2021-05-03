# raft
分布式 raft 共识算法 go 实现

### 一、功能

#### 领导者选举
* 选举超时时间取 `ElectionMinTimeout` 和 `ElectionMaxTimeout` 之间的一个随机数，可在 `raft.Config` 中设置
* Pre-Vote 机制，在候选者开启新一轮选举之前，会确定是否可获得多数投票，避免 `term` 值无意义地增加

#### 日志复制
* 领导者并发地向所有追随者发送日志，当超过半数的节点（包括自己）成功保存日志后，领导者进行日志提交，追随者在接收到下一次心跳后提交日志
* 如果追随者日志落后，领导者视情况发送快照或日志给追随者

#### 日志压缩
* 使用快照来进行日志的压缩，领导者和追随者各自独立进行
* 根据内存中日志量大小来判断是否进行压缩，由 `MaxLogLength` 决定，在 `raft.Config` 中设置

#### 领导权转移
* 由客户端决定需要晋升为领导者的节点
* 若待晋升的节点日志落后于领导者，则先进行日志追赶
* 日志进度追赶成功后，领导者向待晋升节点发送一个选举立即超时命令
* 领导权转移期间，集群处于不可用状态

#### Learner 节点
* 空白节点启动时，可指定节点角色为 `Learner`，此角色的节点不参与选举投票
* 领导者向 `Learner` 发送快照或日志，进行日志追赶，追随者对此节点无感知

#### 成员变更
* 使用 `joint consensus` 进行成员变更，成员变更期间，集群不可用
* 若新配置的节点中包含先前添加的 `Learner` 节点，则先晋升为 `Follower` 节点

### 二、需要实现的接口

**Note：bitcapybara/raft 只实现了 raft 算法逻辑，而存储和网络相关的实现通过接口的方式开放给客户端定制。**

#### Fsm

> 客户端状态机接口，在 raft 内部调用此接口来实现状态机的相关操作，比如应用日志，生成快照，安装快照等。

#### Transport

> 在 raft 内部调用此接口的各个方法用于网络通信，比如发送心跳，日志复制，领导者选举，发送快照等。

#### RaftStatePersister

> 在 raft 内部调用此接口来持久化和加载内部状态数据，包括 term，votedFor及日志条目。

#### SnapshotPersister

> 在 raft 内部调用此接口来持久化和加载快照数据。

#### Logger

> 在 raft 内部调用此接口来打印日志。

**接口实现后，通过 raft.Config 传入即可**

### 三、使用

1. 新建一个 `raft.Node` 对象，代表当前节点
2. 使用 `raft.Node.Run()` 方法开启 raft 循环
3. 在开放 HTTP/RPC 接口中调用 `raft.Node` 的相应方法来接收来自其它节点的 raft 网络请求

### 四、示例

[simplefsm](https://github.com/bitcapybara/simplefsm) 项目是此 raft 库的一个示例，实现了一个极简的状态机，但已经包含了此 raft 库的所有功能。

