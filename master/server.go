// Copyright 2018 The Chubao Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"context"
	"fmt"
	syslog "log"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"sync"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/cryptoutil"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
)

// configuration keys
const (
	ClusterName        = "clusterName"
	ID                 = "id"
	IP                 = "ip"
	Port               = "port"
	LogLevel           = "logLevel"
	WalDir             = "walDir"
	StoreDir           = "storeDir"
	GroupID            = 1
	ModuleName         = "master"
	CfgRetainLogs      = "retainLogs"
	DefaultRetainLogs  = 20000
	cfgTickInterval    = "tickInterval"
	cfgRaftRecvBufSize = "raftRecvBufSize"
	cfgElectionTick    = "electionTick"
	SecretKey          = "masterServiceKey"
)

var (
	// regexps for data validation
	volNameRegexp = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,61}[a-zA-Z0-9]$")
	ownerRegexp   = regexp.MustCompile("^[A-Za-z][A-Za-z0-9_]{0,20}$")

	useConnPool = true //for test
	gConfig     *clusterConfig
)

// Server represents the server in a cluster
type Server struct {
	id              uint64
	clusterName     string
	ip              string
	port            string
	walDir          string
	storeDir        string
	retainLogs      uint64
	tickInterval    int
	raftRecvBufSize int
	electionTick    int
	leaderInfo      *LeaderInfo
	config          *clusterConfig
	cluster         *Cluster
	user            *User
	rocksDBStore    *raftstore.RocksDBStore
	raftStore       raftstore.RaftStore
	fsm             *MetadataFsm
	partition       raftstore.Partition
	wg              sync.WaitGroup
	reverseProxy    *httputil.ReverseProxy
	metaReady       bool
	apiServer       *http.Server
}

// NewServer creates a new server
func NewServer() *Server {
	return &Server{}
}

// Start starts a server
func (m *Server) Start(cfg *config.Config) (err error) {
	// m是对象Server的引用，启动就是Server这个对象的启动
	// 以下操作都是对m这个对象的字段进行赋值

	// 先进行clusterConfig的初始化，初始化一些启动参数，生成config对象
	m.config = newClusterConfig()
	// 然后把上一步生成的参数对象赋值给gConfig，也就是此类的变量
	gConfig = m.config
	// 创建一个对象leaderinfo，只包含了addr信息，也就是一些ip和port地址信息
	m.leaderInfo = &LeaderInfo{}
	// 创建反向代理服务器对象，并把信息放在reverseProxy这个里
	m.reverseProxy = m.newReverseProxy()
	// 检查配置的参数是否有问题，若有问题就抛出error，并返回，也就是启动失败
	// 这里的配置检查，会在生成raftserver时再次检查，但是检查的范围不一致
	if err = m.checkConfig(cfg); err != nil {
		log.LogError(errors.Stack(err))
		return
	}

	// 生成rocksDB对象
	if m.rocksDBStore, err = raftstore.NewRocksDBStore(m.storeDir, LRUCacheSize, WriteBufferSize); err != nil {
		return
	}

	// 检查配置参数，并生成raftserver之后，赋值到对象字段中
	if err = m.createRaftServer(); err != nil {
		log.LogError(errors.Stack(err))
		return
	}

	// 初始化集群信息
	m.initCluster()
	// 初始化用户权限
	m.initUser()
	m.cluster.partition = m.partition
	m.cluster.idAlloc.partition = m.partition
	MasterSecretKey := cfg.GetString(SecretKey)
	if m.cluster.MasterSecretKey, err = cryptoutil.Base64Decode(MasterSecretKey); err != nil {
		return fmt.Errorf("action[Start] failed %v, err: master service Key invalid = %s", proto.ErrInvalidCfg, MasterSecretKey)
	}
	// 这里主要是开启一些定时任务，可以找开发咨询下有哪些定时任务，要一些主要的定时任务，讲解时大概说一下即可
	m.cluster.scheduleTask()
	// 启动对外提供api服务，方便进行管理和请求数据
	m.startHTTPService(ModuleName, cfg)
	exporter.RegistConsul(m.clusterName, ModuleName, cfg)

	// 增加监控，监控项可以找开发咨询下，讲时可以列举一两个说加了这些监控等等
	metricsService := newMonitorMetrics(m.cluster)
	metricsService.start()
	// 利用计数器来让主协程等待其他协程执行完成，防止被关闭
	m.wg.Add(1)
	return nil
}

// Shutdown closes the server
func (m *Server) Shutdown() {
	var err error
	if m.apiServer != nil {
		if err = m.apiServer.Shutdown(context.Background()); err != nil {
			log.LogErrorf("action[Shutdown] failed, err: %v", err)
		}
	}
	m.wg.Done()
}

// Sync waits for the execution termination of the server
func (m *Server) Sync() {
	m.wg.Wait()
}

func (m *Server) checkConfig(cfg *config.Config) (err error) {
	m.clusterName = cfg.GetString(ClusterName)
	m.ip = cfg.GetString(IP)
	m.port = cfg.GetString(proto.ListenPort)
	m.walDir = cfg.GetString(WalDir)
	m.storeDir = cfg.GetString(StoreDir)
	peerAddrs := cfg.GetString(cfgPeers)
	// 若以上配置有一项为空，那么就会报错
	if m.ip == "" || m.port == "" || m.walDir == "" || m.storeDir == "" || m.clusterName == "" || peerAddrs == "" {
		return fmt.Errorf("%v,err:%v,%v,%v,%v,%v,%v,%v", proto.ErrInvalidCfg, "one of (ip,listen,walDir,storeDir,clusterName) is null",
			m.ip, m.port, m.walDir, m.storeDir, m.clusterName, peerAddrs)
	}
	if m.id, err = strconv.ParseUint(cfg.GetString(ID), 10, 64); err != nil {
		return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
	}
	m.config.faultDomain = cfg.GetBoolWithDefault(faultDomain, false)
	m.config.heartbeatPort = cfg.GetInt64(heartbeatPortKey)
	m.config.replicaPort = cfg.GetInt64(replicaPortKey)
	// 若心跳的端口小于1024是不允许的，就必须改为默认值5901
	if m.config.heartbeatPort <= 1024 {
		m.config.heartbeatPort = raftstore.DefaultHeartbeatPort
	}
	// 若复制的端口小于1024是不允许的，就必须改为默认值5902
	if m.config.replicaPort <= 1024 {
		m.config.replicaPort = raftstore.DefaultReplicaPort
	}
	syslog.Printf("heartbeatPort[%v],replicaPort[%v]\n", m.config.heartbeatPort, m.config.replicaPort)
	if err = m.config.parsePeers(peerAddrs); err != nil {
		return
	}
	nodeSetCapacity := cfg.GetString(nodeSetCapacity)
	if nodeSetCapacity != "" {
		if m.config.nodeSetCapacity, err = strconv.Atoi(nodeSetCapacity); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}
	if m.config.nodeSetCapacity < 3 {
		m.config.nodeSetCapacity = defaultNodeSetCapacity
	}

	m.config.DomainBuildAsPossible = cfg.GetBoolWithDefault(cfgDomainBuildAsPossible, false)
	m.config.DomainNodeGrpBatchCnt = defaultNodeSetGrpBatchCnt
	domainBatchGrpCnt := cfg.GetString(cfgDomainBatchGrpCnt)
	if domainBatchGrpCnt != "" {
		if m.config.DomainNodeGrpBatchCnt, err = strconv.Atoi(domainBatchGrpCnt); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}

	metaNodeReservedMemory := cfg.GetString(cfgMetaNodeReservedMem)
	if metaNodeReservedMemory != "" {
		if m.config.metaNodeReservedMem, err = strconv.ParseUint(metaNodeReservedMemory, 10, 64); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}
	// metaNode节点若少于32MB的话，就改为默认值1G
	if m.config.metaNodeReservedMem < 32*1024*1024 {
		m.config.metaNodeReservedMem = defaultMetaNodeReservedMem
	}

	retainLogs := cfg.GetString(CfgRetainLogs)
	if retainLogs != "" {
		if m.retainLogs, err = strconv.ParseUint(retainLogs, 10, 64); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}
	// log保存数量若小于0，会改为默认值，20000
	if m.retainLogs <= 0 {
		m.retainLogs = DefaultRetainLogs
	}
	syslog.Println("retainLogs=", m.retainLogs)

	// if the data partition has not been reported within this interval  (in terms of seconds), it will be considered as missing
	// missingDataPartitionInterval此值代表若数据节点在间隔missingDataPartitionInterval时间内没有心跳，就会认为数据节点挂了
	missingDataPartitionInterval := cfg.GetString(missingDataPartitionInterval)
	if missingDataPartitionInterval != "" {
		if m.config.MissingDataPartitionInterval, err = strconv.ParseInt(missingDataPartitionInterval, 10, 0); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}

	dataPartitionTimeOutSec := cfg.GetString(dataPartitionTimeOutSec)
	if dataPartitionTimeOutSec != "" {
		if m.config.DataPartitionTimeOutSec, err = strconv.ParseInt(dataPartitionTimeOutSec, 10, 0); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}

	numberOfDataPartitionsToLoad := cfg.GetString(NumberOfDataPartitionsToLoad)
	if numberOfDataPartitionsToLoad != "" {
		if m.config.numberOfDataPartitionsToLoad, err = strconv.Atoi(numberOfDataPartitionsToLoad); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}
	if m.config.numberOfDataPartitionsToLoad <= 40 {
		m.config.numberOfDataPartitionsToLoad = 40
	}
	if secondsToFreeDP := cfg.GetString(secondsToFreeDataPartitionAfterLoad); secondsToFreeDP != "" {
		if m.config.secondsToFreeDataPartitionAfterLoad, err = strconv.ParseInt(secondsToFreeDP, 10, 64); err != nil {
			return fmt.Errorf("%v,err:%v", proto.ErrInvalidCfg, err.Error())
		}
	}
	m.tickInterval = int(cfg.GetFloat(cfgTickInterval))
	m.raftRecvBufSize = int(cfg.GetInt(cfgRaftRecvBufSize))
	m.electionTick = int(cfg.GetFloat(cfgElectionTick))
	if m.tickInterval <= 300 {
		m.tickInterval = 500
	}
	if m.electionTick <= 3 {
		m.electionTick = 5
	}
	return
}

func (m *Server) createRaftServer() (err error) {
	raftCfg := &raftstore.Config{
		NodeID:            m.id,
		RaftPath:          m.walDir,
		NumOfLogsToRetain: m.retainLogs,
		HeartbeatPort:     int(m.config.heartbeatPort),
		ReplicaPort:       int(m.config.replicaPort),
		TickInterval:      m.tickInterval,
		ElectionTick:      m.electionTick,
		RecvBufSize:       m.raftRecvBufSize,
	}
	if m.raftStore, err = raftstore.NewRaftStore(raftCfg); err != nil {
		return errors.Trace(err, "NewRaftStore failed! id[%v] walPath[%v]", m.id, m.walDir)
	}
	syslog.Printf("peers[%v],tickInterval[%v],electionTick[%v]\n", m.config.peers, m.tickInterval, m.electionTick)
	// 这里开始初始化metaDataFsm对象，会把rocksDB对象、raftserver对象、log日志等赋值给metadatafsm对象
	m.initFsm()
	partitionCfg := &raftstore.PartitionConfig{
		ID:      GroupID,
		Peers:   m.config.peers,
		Applied: m.fsm.applied,
		SM:      m.fsm,
	}
	if m.partition, err = m.raftStore.CreatePartition(partitionCfg); err != nil {
		return errors.Trace(err, "CreatePartition failed")
	}
	return
}
func (m *Server) initFsm() {
	// 生成MetadataFsm对象，并把rocksDB、raftServer等值赋值给此对象字段
	m.fsm = newMetadataFsm(m.rocksDBStore, m.retainLogs, m.raftStore.RaftServer())
	// 给MetadataFsm对象赋值一些处理器，如leader选举变更处理器、Follower节点变更处理器
	m.fsm.registerLeaderChangeHandler(m.handleLeaderChange)
	m.fsm.registerPeerChangeHandler(m.handlePeerChange)

	// register the handlers for the interfaces defined in the Raft library
	// 注册以下接口，主要是为了定义raft库开放的一些接口，方便处理相应事件
	m.fsm.registerApplySnapshotHandler(m.handleApplySnapshot)
	m.fsm.registerRaftUserCmdApplyHandler(m.handleRaftUserCmd)
	m.fsm.restore()
}

func (m *Server) initCluster() {
	m.cluster = newCluster(m.clusterName, m.leaderInfo, m.fsm, m.partition, m.config)
	m.cluster.retainLogs = m.retainLogs
}

func (m *Server) initUser() {
	m.user = newUser(m.fsm, m.partition)
}
