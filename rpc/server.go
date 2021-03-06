package rpc

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
)

import (
	"github.com/AlexStocks/getty"
	"github.com/AlexStocks/goext/database/registry"
	"github.com/AlexStocks/goext/database/registry/etcdv3"
	"github.com/AlexStocks/goext/database/registry/zookeeper"
	"github.com/AlexStocks/goext/net"
	log "github.com/AlexStocks/log4go"
	jerrors "github.com/juju/errors"
)

type Server struct {
	conf          *ServerConfig
	serviceMap    map[string]*service
	tcpServerList []getty.Server
	registry      gxregistry.Registry
	sa            gxregistry.ServiceAttr
	nodes         []*gxregistry.Node
}

var (
	ErrIllegalCodecType = jerrors.New("illegal codec type")
)

func NewServer(confFile string) (*Server, error) {
	conf := loadServerConf(confFile)
	if conf.codecType = String2CodecType(conf.CodecType); conf.codecType == gettyCodecUnknown {
		return nil, ErrIllegalCodecType
	}

	s := &Server{
		serviceMap: make(map[string]*service),
		conf:       conf,
	}

	var err error
	var registry gxregistry.Registry
	if len(s.conf.Registry.Addr) != 0 {
		addrList := strings.Split(s.conf.Registry.Addr, ",")
		switch s.conf.Registry.Type {
		case "etcd":
			registry, err = gxetcd.NewRegistry(
				gxregistry.WithAddrs(addrList...),
				gxregistry.WithTimeout(time.Duration(int(time.Second)*s.conf.Registry.KeepaliveTimeout)),
				gxregistry.WithRoot(s.conf.Registry.Root),
			)
		case "zookeeper":
			registry, err = gxzookeeper.NewRegistry(
				gxregistry.WithAddrs(addrList...),
				gxregistry.WithTimeout(time.Duration(int(time.Second)*s.conf.Registry.KeepaliveTimeout)),
				gxregistry.WithRoot(s.conf.Registry.Root),
			)
		}

		if err != nil {
			return nil, jerrors.Trace(err)
		}
		if registry != nil {
			s.registry = registry
			s.sa = gxregistry.ServiceAttr{
				Group:    s.conf.Registry.IDC,
				Role:     gxregistry.SRT_Provider,
				Protocol: s.conf.CodecType,
			}

			for _, p := range s.conf.Ports {
				port, err := strconv.Atoi(p)
				if err != nil {
					return nil, jerrors.New(fmt.Sprintf("illegal port %s", p))
				}

				s.nodes = append(s.nodes,
					&gxregistry.Node{
						ID:      s.conf.Registry.NodeID + "-" + net.JoinHostPort(s.conf.Host, p),
						Address: s.conf.Host,
						Port:    int32(port)})
			}
		}
	}

	return s, nil
}

func (s *Server) Run() {
	s.Init()
	log.Info("%s starts successfull! its version=%s, its listen ends=%s:%s\n",
		s.conf.AppName, getty.Version, s.conf.Host, s.conf.Ports)
	s.initSignal()
}

func (s *Server) Register(rcvr GettyRPCService) error {
	svc := &service{
		typ:  reflect.TypeOf(rcvr),
		rcvr: reflect.ValueOf(rcvr),
		name: reflect.Indirect(reflect.ValueOf(rcvr)).Type().Name(),
		// Install the methods
		method: suitableMethods(reflect.TypeOf(rcvr)),
	}
	if svc.name == "" {
		s := "rpc.Register: no service name for type " + svc.typ.String()
		log.Error(s)
		return jerrors.New(s)
	}
	if !isExported(svc.name) {
		s := "rpc.Register: type " + svc.name + " is not exported"
		log.Error(s)
		return jerrors.New(s)
	}
	if _, present := s.serviceMap[svc.name]; present {
		return jerrors.New("rpc: service already defined: " + svc.name)
	}

	if len(svc.method) == 0 {
		// To help the user, see if a pointer receiver would work.
		method := suitableMethods(reflect.PtrTo(svc.typ))
		str := "rpc.Register: type " + svc.name + " has no exported methods of suitable type"
		if len(method) != 0 {
			str = "rpc.Register: type " + svc.name + " has no exported methods of suitable type (" +
				"hint: pass a pointer to value of that type)"
		}
		log.Error(str)

		return jerrors.New(str)
	}

	s.serviceMap[svc.name] = svc
	if s.registry != nil {
		sa := s.sa
		sa.Service = rcvr.Service()
		sa.Version = rcvr.Version()
		service := gxregistry.Service{Attr: &sa, Nodes: s.nodes}
		if err := s.registry.Register(service); err != nil {
			return jerrors.Trace(err)
		}
	}

	return nil
}

func (s *Server) newSession(session getty.Session) error {
	var (
		ok      bool
		tcpConn *net.TCPConn
	)

	if s.conf.GettySessionParam.CompressEncoding {
		session.SetCompressType(getty.CompressZip)
	}

	if tcpConn, ok = session.Conn().(*net.TCPConn); !ok {
		panic(fmt.Sprintf("%s, session.conn{%#v} is not tcp connection\n", session.Stat(), session.Conn()))
	}

	tcpConn.SetNoDelay(s.conf.GettySessionParam.TcpNoDelay)
	tcpConn.SetKeepAlive(s.conf.GettySessionParam.TcpKeepAlive)
	if s.conf.GettySessionParam.TcpKeepAlive {
		tcpConn.SetKeepAlivePeriod(s.conf.GettySessionParam.keepAlivePeriod)
	}
	tcpConn.SetReadBuffer(s.conf.GettySessionParam.TcpRBufSize)
	tcpConn.SetWriteBuffer(s.conf.GettySessionParam.TcpWBufSize)

	session.SetName(s.conf.GettySessionParam.SessionName)
	session.SetMaxMsgLen(s.conf.GettySessionParam.MaxMsgLen)
	session.SetPkgHandler(NewRpcServerPackageHandler(s))
	session.SetEventListener(NewRpcServerHandler(s.conf.SessionNumber, s.conf.sessionTimeout))
	session.SetRQLen(s.conf.GettySessionParam.PkgRQSize)
	session.SetWQLen(s.conf.GettySessionParam.PkgWQSize)
	session.SetReadTimeout(s.conf.GettySessionParam.tcpReadTimeout)
	session.SetWriteTimeout(s.conf.GettySessionParam.tcpWriteTimeout)
	session.SetCronPeriod((int)(s.conf.sessionTimeout.Nanoseconds() / 1e6))
	session.SetWaitTime(s.conf.GettySessionParam.waitTimeout)
	log.Debug("app accepts new session:%s\n", session.Stat())

	return nil
}

func (s *Server) Init() {
	var (
		addr      string
		portList  []string
		tcpServer getty.Server
	)

	portList = s.conf.Ports
	if len(portList) == 0 {
		panic("portList is nil")
	}
	for _, port := range portList {
		addr = gxnet.HostAddress2(s.conf.Host, port)
		tcpServer = getty.NewTCPServer(
			getty.WithLocalAddress(addr),
		)
		tcpServer.RunEventLoop(s.newSession)
		log.Debug("s bind addr{%s} ok!", addr)
		s.tcpServerList = append(s.tcpServerList, tcpServer)
	}
}

func (s *Server) Stop() {
	for _, tcpServer := range s.tcpServerList {
		tcpServer.Close()
	}
}

func (s *Server) initSignal() {
	signals := make(chan os.Signal, 1)
	// It is impossible to block SIGKILL or syscall.SIGSTOP
	signal.Notify(signals, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	for {
		sig := <-signals
		log.Info("get signal %s", sig.String())
		switch sig {
		case syscall.SIGHUP:
		// reload()
		default:
			go time.AfterFunc(s.conf.failFastTimeout, func() {
				log.Exit("app exit now by force...")
				log.Close()
			})

			// if @s can not stop in s.conf.failFastTimeout, getty will Force Quit.
			s.Stop()
			log.Exit("app exit now...")
			log.Close()
			return
		}
	}
}
