package proxy

import (
	errs "errors"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"overlord/pkg/log"
	libnet "overlord/pkg/net"
	"overlord/pkg/types"
	"overlord/proxy/proto"
	"overlord/proxy/proto/memcache"
	mcbin "overlord/proxy/proto/memcache/binary"
	"overlord/proxy/proto/redis"
	rclstr "overlord/proxy/proto/redis/cluster"

	"github.com/pkg/errors"
)

// proxy errors
var (
	ErrProxyMoreMaxConns              = errs.New("Proxy accept more than max connextions")
	ClusterSn                   int32 = 0
	MonitorCfgIntervalMilliSecs int   = 10 * 100 // Time interval to monitor config change
	ClusterChangeCount          int32 = 0
	ClusterConfChangeFailCnt    int32 = 0
	AddClusterFailCnt           int32 = 0
	LoadFailCnt                 int32 = 0
	FailedDueToRemovedCnt       int32 = 0
)

const MaxClusterCnt int32 = 128

type Cluster struct {
	conf        *ClusterConfig
	clientConns map[int64]*libnet.Conn
	forwarder   proto.Forwarder
	mutex       sync.Mutex
}

// Proxy is proxy.
type Proxy struct {
	c               *Config
	ClusterConfFile string // cluster configure file name

	clusters      [MaxClusterCnt]*Cluster
	CurClusterCnt int32
	once          sync.Once

	conns int32

	lock   sync.Mutex
	closed bool
}

// New new a proxy by config.
func NewProxy(c *Config) (p *Proxy, err error) {
	if err = c.Validate(); err != nil {
		err = errors.Wrap(err, "Proxy New config validate error")
		return
	}
	p = &Proxy{}
	p.c = c
	return
}

func genClusterSn() int32 {
	var id = atomic.AddInt32(&ClusterSn, 1)
	return id
}

// Serve is the main accept() loop of a server.
func (p *Proxy) Serve(ccs []*ClusterConfig) {
	p.once.Do(func() {
		if len(ccs) == 0 {
			log.Warnf("overlord will never listen on any port due to cluster is not specified")
		}
		p.CurClusterCnt = 0
		for _, conf := range ccs {
			var err = p.addCluster(conf)
			if err != nil {
				// it is safety to panic here as it in start up logic
				panic(err)
			}
		}
		// Here, start a go routine to change content of a forwarder
		go p.monitorConfChange()
	})
}

func (p *Proxy) addCluster(newConf *ClusterConfig) error {
	newConf.SN = genClusterSn()
	p.lock.Lock()
	var clusterID = p.CurClusterCnt
	newConf.ID = clusterID
	var newForwarder, err = NewForwarder(newConf)
	if err != nil {
		p.lock.Unlock()
		return err
	}
	newForwarder.AddRef()
	var cluster = &Cluster{conf: newConf, forwarder: newForwarder}
	cluster.clientConns = make(map[int64]*libnet.Conn)
	p.clusters[clusterID] = cluster
	var servErr = p.serve(clusterID)
	if servErr != nil {
		p.clusters[clusterID] = nil
		p.lock.Unlock()
		cluster.Close()
		cluster.forwarder = nil
		newForwarder.Release()
		return servErr
	}
	p.CurClusterCnt++
	p.lock.Unlock()
	log.Infof("succeed to add cluster:%s with addr:%s\n", newConf.Name, newConf.ListenAddr)
	return nil
}

func (p *Proxy) serve(cid int32) error {
	// listen
	var conf = p.getClusterConf(cid)
	l, err := Listen(conf.ListenProto, conf.ListenAddr)
	if err != nil {
		log.Errorf("failed to listen on address:%s, got error:%s\n", conf.ListenAddr, err.Error())
		return err
	}
	log.Infof("overlord proxy cluster[%s] addr(%s) start listening", conf.Name, conf.ListenAddr)
	go p.accept(cid, l)
	return nil
}

func (p *Proxy) accept(cid int32, l net.Listener) {
	for {
		var conf = p.getClusterConf(cid)
		if p.closed {
			log.Infof("overlord proxy cluster[%s] addr(%s) stop listen", conf.Name, conf.ListenAddr)
			return
		}
		conn, err := l.Accept()
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}
			log.Errorf("cluster(%s) addr(%s) accept connection error:%+v", conf.Name, conf.ListenAddr, err)
			continue
		}
		if p.c.Proxy.MaxConnections > 0 {
			if conns := atomic.LoadInt32(&p.conns); conns > p.c.Proxy.MaxConnections {
				// cache type
				var encoder proto.ProxyConn
				switch conf.CacheType {
				case types.CacheTypeMemcache:
					encoder = memcache.NewProxyConn(libnet.NewConn(conn, time.Second, time.Second))
				case types.CacheTypeMemcacheBinary:
					encoder = mcbin.NewProxyConn(libnet.NewConn(conn, time.Second, time.Second))
				case types.CacheTypeRedis:
					encoder = redis.NewProxyConn(libnet.NewConn(conn, time.Second, time.Second))
				case types.CacheTypeRedisCluster:
					encoder = rclstr.NewProxyConn(libnet.NewConn(conn, time.Second, time.Second))
				}
				if encoder != nil {
					var f = p.GetForwarder(cid)
					_ = encoder.Encode(proto.ErrMessage(ErrProxyMoreMaxConns), f)
					_ = encoder.Flush()
					f.Release()
				}
				_ = conn.Close()
				if log.V(4) {
					log.Warnf("proxy reject connection count(%d) due to more than max(%d)", conns, p.c.Proxy.MaxConnections)
				}
				continue
			}
		}
		atomic.AddInt32(&p.conns, 1)
		var frontConn = libnet.NewConn(conn, time.Second*time.Duration(p.c.Proxy.ReadTimeout), time.Second*time.Duration(p.c.Proxy.WriteTimeout))
		err = p.addConnection(cid, conf.SN, frontConn)
		if err != nil {
			// corner case, configure changed when we try to keep this connection
			log.Errorf("corner case, configure just changed when after accept a connection, got error:%s\n", err.Error())
			frontConn.Close()
			continue
		}
		NewHandler(p, conf, frontConn).Handle()
	}
}

// Close close proxy resource.
func (p *Proxy) Close() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.closed {
		return nil
	}
	for i := 0; i < int(p.CurClusterCnt); i++ {
		p.clusters[i].Close()
	}
	p.closed = true
	return nil
}

// Get forwarder from proxy, thread safe
func (p *Proxy) addConnection(cid int32, sn int32, conn *libnet.Conn) error {
	var ret = p.clusters[cid].addConnection(sn, conn)
	return ret
}

func (p *Proxy) RemoveConnection(cid int32, connID int64) {
	p.clusters[cid].removeConnection(connID)
}

func (p *Proxy) CloseAndRemoveConnection(cid int32, connID int64) {
	p.clusters[cid].closeAndRemoveConnection(connID)
}

func (p *Proxy) CloseAllConnections(cid int32) {
	p.clusters[cid].closeAllConnections()
}

// Get forwarder from proxy, thread safe
func (p *Proxy) GetForwarder(cid int32) proto.Forwarder {
	return p.clusters[cid].getForwarder()
}

// Get forwarder from proxy, thread safe
func (p *Proxy) getClusterConf(cid int32) *ClusterConfig {
	return p.clusters[cid].getConf()
}

func (p *Proxy) anyClusterRemoved(newConfs, oldConfs []*ClusterConfig) bool {
	var (
		newNames = make(map[string]int)
		oldNames = make(map[string]int)
	)
	for _, conf := range newConfs {
		newNames[conf.Name] = 1
	}
	for _, conf := range oldConfs {
		oldNames[conf.Name] = 1
	}
	for name, _ := range oldNames {
		_, find := newNames[name]
		if !find {
			return true
		}
	}
	return false
}

func (p *Proxy) parseChanged(newConfs, oldConfs []*ClusterConfig) (changed, newAdd []*ClusterConfig) {
	for _, newConf := range newConfs {
		var find = false
		var diff = false
		var oldConf *ClusterConfig = nil
		var valid = true
		for _, conf := range oldConfs {
			if newConf.Name != conf.Name {
				// log.Infof("confname is different, %s VS %s\n", newConf.Name, conf.Name)
				continue
			}
			find = true
			diff, valid = compareConf(conf, newConf)
			if !valid {
				log.Errorf("configure change of cluster(%s) is invalid", newConf.Name)
				break
			}
			if diff {
				oldConf = conf
			}
			break
		}
		if !find {
			newAdd = append(newAdd, newConf)
			continue
		}
		if (!valid) || (find && !diff) {
			continue
		}
		newConf.ID = oldConf.ID
		changed = append(changed, newConf)
		continue
	}
	// log.Infof("new conf len:%d old conf len:%d\n", len(newConfs), len(oldConfs))
	return changed, newAdd
}

func (p *Proxy) monitorConfChange() {
	for {
		time.Sleep(time.Duration(MonitorCfgIntervalMilliSecs) * time.Millisecond)
		var newConfs, err = LoadClusterConf(p.ClusterConfFile)
		if err != nil {
			log.Errorf("failed to load conf file:%s, got error:%s\n", p.ClusterConfFile, err.Error())
			atomic.AddInt32(&LoadFailCnt, 1)
			continue
		}
		var oldConfs []*ClusterConfig
		for i := int32(0); i < p.CurClusterCnt; i++ {
			var conf = p.clusters[i].getConf()
			oldConfs = append(oldConfs, conf)
		}
		var removed = p.anyClusterRemoved(newConfs, oldConfs)
		if removed {
			log.Errorf("some cluster is removed from conf file, ignore this change")
			atomic.AddInt32(&FailedDueToRemovedCnt, 1)
			continue
		}

		var newAdd []*ClusterConfig
		var changedConf []*ClusterConfig
		changedConf, newAdd = p.parseChanged(newConfs, oldConfs)

		var clusterCnt = p.CurClusterCnt + int32(len(newAdd))

		if clusterCnt > MaxClusterCnt {
			log.Errorf("failed to reload conf as too much cluster will be added, new cluster count(%d) and max count(%d)",
				clusterCnt, MaxClusterCnt)
			continue
		}
		for _, conf := range changedConf {
			// use new forwarder now
			var err = p.clusters[conf.ID].processConfChange(conf)
			if err == nil {
				atomic.AddInt32(&ClusterChangeCount, 1)
				log.Infof("succeed to change conf of cluster(%s:%d)\n", conf.Name, conf.ID)
				continue
			}
			atomic.AddInt32(&ClusterConfChangeFailCnt, 1)
			log.Errorf("failed to change conf of cluster(%s), got error:%s\n", conf.Name, err.Error())
		}
		for _, conf := range newAdd {
			var err = p.addCluster(conf)
			if err != nil {
				atomic.AddInt32(&AddClusterFailCnt, 1)
				log.Errorf("failed to add new cluster:%s, got error:%s\n", conf.Name, err.Error())
				continue
			}
			log.Infof("succeed to add new cluster:%s", conf.Name)
		}
	}
}

func (c *Cluster) Close() {
	c.forwarder.Close()
	c.closeAllConnections()
}

func (c *Cluster) addConnection(sn int32, conn *libnet.Conn) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if sn != c.conf.SN {
		return errors.New("config is change, try again from:" + strconv.Itoa(int(sn)) + " to:" + strconv.Itoa(int(c.conf.SN)))
	}
	c.clientConns[conn.ID] = conn
	return nil
}

func (c *Cluster) removeConnection(id int64) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	delete(c.clientConns, id)
}

func (c *Cluster) closeAndRemoveConnection(id int64) {
	c.mutex.Lock()
	var conn, ok = c.clientConns[id]
	if !ok {
		c.mutex.Unlock()
		return
	}
	delete(c.clientConns, id)
	c.mutex.Unlock()
	conn.Close()
}

func (c *Cluster) closeAllConnections() {
	c.mutex.Lock()
	var curConns = c.clientConns
	c.clientConns = make(map[int64]*libnet.Conn)
	c.mutex.Unlock()
	for _, conn := range curConns {
		conn.Close()
	}
}

func (c *Cluster) processConfChange(newConf *ClusterConfig) error {
	newConf.ID = c.conf.ID
	newConf.SN = genClusterSn()
	var newForwarder, err = NewForwarder(newConf)
	if err != nil {
		return err
	}
	newForwarder.AddRef()
	c.mutex.Lock()
	var oldConns = c.clientConns
	var oldForwarder = c.forwarder
	c.forwarder = newForwarder
	if newConf.CloseWhenChange {
		c.clientConns = make(map[int64]*libnet.Conn)
	}
	c.conf = newConf
	c.mutex.Unlock()
	oldForwarder.Close()
	oldForwarder.Release()
	if newConf.CloseWhenChange {
		for _, conn := range oldConns {
			conn.Close()
		}
	}
	return nil
}

func (c *Cluster) getForwarder() proto.Forwarder {
	c.mutex.Lock()
	var f = c.forwarder
	c.mutex.Unlock()
	f.AddRef()
	return f
}

func (c *Cluster) getConf() *ClusterConfig {
	c.mutex.Lock()
	var conf = c.conf
	c.mutex.Unlock()
	return conf
}

func compareConf(oldConf, newConf *ClusterConfig) (changed, valid bool) {
	valid = (oldConf.ListenAddr == newConf.ListenAddr)
	if ((oldConf.HashMethod != newConf.HashMethod) ||
		(oldConf.HashDistribution != newConf.HashDistribution) ||
		(oldConf.HashTag != newConf.HashTag) ||
		(oldConf.CacheType != newConf.CacheType) ||
		(oldConf.ListenProto != newConf.ListenProto) ||
		(oldConf.RedisAuth != newConf.RedisAuth) ||
		(oldConf.DialTimeout != newConf.DialTimeout) ||
		(oldConf.ReadTimeout != newConf.ReadTimeout) ||
		(oldConf.WriteTimeout != newConf.WriteTimeout) ||
		(oldConf.NodeConnections != newConf.NodeConnections) ||
		(oldConf.PingFailLimit != newConf.PingFailLimit) ||
		(oldConf.PingAutoEject != newConf.PingAutoEject)) ||
		(oldConf.CloseWhenChange != newConf.CloseWhenChange) {
		changed = true
		return
	}
	if len(oldConf.Servers) != len(newConf.Servers) {
		changed = true
		return
	}
	var server1 = oldConf.Servers
	var server2 = newConf.Servers
	sort.Strings(server1)
	sort.Strings(server2)
	// var str1 = strings.Join(server1, "_")
	// var str2 = strings.Join(server2, "_")
	// log.Infof("server1:%s server2:%s\n", str1, str2)
	for i := 0; i < len(server1); i++ {
		if server1[i] != server2[i] {
			changed = true
			return
		}
	}
	changed = false
	return
}
