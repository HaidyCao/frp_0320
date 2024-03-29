// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/HaidyCao/frp_0320/assets"
	"github.com/HaidyCao/frp_0320/models/auth"
	"github.com/HaidyCao/frp_0320/models/config"
	modelmetrics "github.com/HaidyCao/frp_0320/models/metrics"
	"github.com/HaidyCao/frp_0320/models/msg"
	"github.com/HaidyCao/frp_0320/models/nathole"
	plugin "github.com/HaidyCao/frp_0320/models/plugin/server"
	"github.com/HaidyCao/frp_0320/server/controller"
	"github.com/HaidyCao/frp_0320/server/group"
	"github.com/HaidyCao/frp_0320/server/metrics"
	"github.com/HaidyCao/frp_0320/server/ports"
	"github.com/HaidyCao/frp_0320/server/proxy"
	"github.com/HaidyCao/frp_0320/utils/log"
	frpNet "github.com/HaidyCao/frp_0320/utils/net"
	"github.com/HaidyCao/frp_0320/utils/tcpmux"
	"github.com/HaidyCao/frp_0320/utils/util"
	"github.com/HaidyCao/frp_0320/utils/version"
	"github.com/HaidyCao/frp_0320/utils/vhost"
	"github.com/HaidyCao/frp_0320/utils/xlog"

	"github.com/fatedier/golib/net/mux"
	fmux "github.com/hashicorp/yamux"
)

const (
	connReadTimeout       time.Duration = 10 * time.Second
	vhostReadWriteTimeout time.Duration = 30 * time.Second
)

// Server service
type Service struct {
	// Dispatch connections to different handlers listen on same port
	muxer *mux.Mux

	// Accept connections from client
	listener net.Listener

	// Accept connections using kcp
	kcpListener net.Listener

	// Accept connections using websocket
	websocketListener net.Listener

	// Accept frp tls connections
	tlsListener net.Listener

	// Manage all controllers
	ctlManager *ControlManager

	// Manage all proxies
	pxyManager *proxy.ProxyManager

	// Manage all plugins
	pluginManager *plugin.Manager

	// HTTP vhost router
	httpVhostRouter *vhost.VhostRouters

	// All resource managers and controllers
	rc *controller.ResourceController

	// Verifies authentication based on selected method
	authVerifier auth.Verifier

	// Closed is service closed
	Closed bool

	// close chan
	closedCh chan bool

	tlsConfig *tls.Config

	cfg config.ServerCommonConf
}

func newAddress(addr string, port int) string {
	if strings.Contains(addr, ".") {
		return fmt.Sprintf("%s:%d", addr, port)
	} else {
		return fmt.Sprintf("[%s]:%d", addr, port)
	}
}

func NewService(cfg config.ServerCommonConf) (svr *Service, err error) {
	svr = &Service{
		ctlManager:    NewControlManager(),
		pxyManager:    proxy.NewProxyManager(),
		pluginManager: plugin.NewManager(),
		rc: &controller.ResourceController{
			VisitorManager: controller.NewVisitorManager(),
			TcpPortManager: ports.NewPortManager("tcp", cfg.ProxyBindAddr, cfg.AllowPorts),
			UdpPortManager: ports.NewPortManager("udp", cfg.ProxyBindAddr, cfg.AllowPorts),
		},
		Closed:          true,
		closedCh:        make(chan bool),
		httpVhostRouter: vhost.NewVhostRouters(),
		authVerifier:    auth.NewAuthVerifier(cfg.AuthServerConfig),
		tlsConfig:       generateTLSConfig(),
		cfg:             cfg,
	}

	// Init all plugins
	for name, options := range cfg.HTTPPlugins {
		svr.pluginManager.Register(plugin.NewHTTPPluginOptions(options))
		log.Info("plugin [%s] has been registered", name)
	}

	// Init group controller
	svr.rc.TcpGroupCtl = group.NewTcpGroupCtl(svr.rc.TcpPortManager)

	// Init HTTP group controller
	svr.rc.HTTPGroupCtl = group.NewHTTPGroupController(svr.httpVhostRouter)

	// Init 404 not found page
	vhost.NotFoundPagePath = cfg.Custom404Page

	var (
		httpMuxOn  bool
		httpsMuxOn bool
	)
	if cfg.BindAddr == cfg.ProxyBindAddr {
		if cfg.BindPort == cfg.VhostHttpPort {
			httpMuxOn = true
		}
		if cfg.BindPort == cfg.VhostHttpsPort {
			httpsMuxOn = true
		}
	}

	// Listen for accepting connections from client.
	ln, err := net.Listen("tcp", newAddress(cfg.BindAddr, cfg.BindPort))
	if err != nil {
		err = fmt.Errorf("Create server listener error, %v", err)
		return
	}

	svr.muxer = mux.NewMux(ln)
	go svr.muxer.Serve()
	ln = svr.muxer.DefaultListener()

	svr.listener = ln
	log.Info("frps tcp listen on %s:%d", cfg.BindAddr, cfg.BindPort)

	// Listen for accepting connections from client using kcp protocol.
	if cfg.KcpBindPort > 0 {
		svr.kcpListener, err = frpNet.ListenKcp(cfg.BindAddr, cfg.KcpBindPort)
		if err != nil {
			err = fmt.Errorf("Listen on kcp address udp [%s:%d] error: %v", cfg.BindAddr, cfg.KcpBindPort, err)
			return
		}
		log.Info("frps kcp listen on udp %s:%d", cfg.BindAddr, cfg.KcpBindPort)
	}

	// Listen for accepting connections from client using websocket protocol.
	websocketPrefix := []byte("GET " + frpNet.FrpWebsocketPath)
	websocketLn := svr.muxer.Listen(0, uint32(len(websocketPrefix)), func(data []byte) bool {
		return bytes.Equal(data, websocketPrefix)
	})
	svr.websocketListener = frpNet.NewWebsocketListener(websocketLn)

	// Create http vhost muxer.
	if cfg.VhostHttpPort > 0 {
		rp := vhost.NewHttpReverseProxy(vhost.HttpReverseProxyOptions{
			ResponseHeaderTimeoutS: cfg.VhostHttpTimeout,
		}, svr.httpVhostRouter)
		svr.rc.HttpReverseProxy = rp

		address := newAddress(cfg.ProxyBindAddr, cfg.VhostHttpPort)
		server := &http.Server{
			Addr:    address,
			Handler: rp,
		}
		var l net.Listener
		if httpMuxOn {
			l = svr.muxer.ListenHttp(1)
		} else {
			l, err = net.Listen("tcp", address)
			if err != nil {
				err = fmt.Errorf("Create vhost http listener error, %v", err)
				return
			}
		}
		go server.Serve(l)
		log.Info("http service listen on %s:%d", cfg.ProxyBindAddr, cfg.VhostHttpPort)
	}

	// Create https vhost muxer.
	if cfg.VhostHttpsPort > 0 {
		var l net.Listener
		if httpsMuxOn {
			l = svr.muxer.ListenHttps(1)
		} else {
			l, err = net.Listen("tcp", newAddress(cfg.ProxyBindAddr, cfg.VhostHttpsPort))
			if err != nil {
				err = fmt.Errorf("Create server listener error, %v", err)
				return
			}
		}

		svr.rc.VhostHttpsMuxer, err = vhost.NewHttpsMuxer(l, vhostReadWriteTimeout)
		if err != nil {
			err = fmt.Errorf("Create vhost httpsMuxer error, %v", err)
			return
		}
		log.Info("https service listen on %s:%d", cfg.ProxyBindAddr, cfg.VhostHttpsPort)
	}

	// Create tcpmux httpconnect multiplexer.
	if cfg.TcpMuxHttpConnectPort > 0 {
		var l net.Listener
		l, err = net.Listen("tcp", fmt.Sprintf("%s:%d", cfg.ProxyBindAddr, cfg.TcpMuxHttpConnectPort))
		if err != nil {
			err = fmt.Errorf("Create server listener error, %v", err)
			return
		}

		svr.rc.TcpMuxHttpConnectMuxer, err = tcpmux.NewHttpConnectTcpMuxer(l, vhostReadWriteTimeout)
		if err != nil {
			err = fmt.Errorf("Create vhost tcpMuxer error, %v", err)
			return
		}
		log.Info("tcpmux httpconnect multiplexer listen on %s:%d", cfg.ProxyBindAddr, cfg.TcpMuxHttpConnectPort)
	}

	// frp tls listener
	svr.tlsListener = svr.muxer.Listen(1, 1, func(data []byte) bool {
		return int(data[0]) == frpNet.FRP_TLS_HEAD_BYTE
	})

	// Create nat hole controller.
	if cfg.BindUdpPort > 0 {
		var nc *nathole.NatHoleController
		addr := newAddress(cfg.BindAddr, cfg.BindUdpPort)
		nc, err = nathole.NewNatHoleController(addr)
		if err != nil {
			err = fmt.Errorf("Create nat hole controller error, %v", err)
			return
		}
		svr.rc.NatHoleController = nc
		log.Info("nat hole udp service listen on %s:%d", cfg.BindAddr, cfg.BindUdpPort)
	}

	var statsEnable bool
	// Create dashboard web server.
	if cfg.DashboardPort > 0 {
		// Init dashboard assets
		err = assets.Load(cfg.AssetsDir, assets.Frps)
		if err != nil {
			err = fmt.Errorf("Load assets error: %v", err)
			return
		}

		err = svr.RunDashboardServer(cfg.DashboardAddr, cfg.DashboardPort)
		if err != nil {
			err = fmt.Errorf("Create dashboard web server error, %v", err)
			return
		}
		log.Info("Dashboard listen on %s:%d", cfg.DashboardAddr, cfg.DashboardPort)
		statsEnable = true
	}
	if statsEnable {
		modelmetrics.EnableMem()
		if cfg.EnablePrometheus {
			modelmetrics.EnablePrometheus()
		}
	}
	return
}

func (svr *Service) Run() {
	if svr.rc.NatHoleController != nil {
		go svr.rc.NatHoleController.Run()
	}
	if svr.cfg.KcpBindPort > 0 {
		go svr.HandleListener(svr.kcpListener)
	}

	go svr.HandleListener(svr.websocketListener)
	go svr.HandleListener(svr.tlsListener)

	svr.Closed = false
	svr.HandleListener(svr.listener)
}

// Stop 停止服务
func (svr *Service) Stop() error {
	var err error
	value := reflect.ValueOf(svr.muxer)
	lnValue := value.Elem().FieldByName("ln")
	ln, ok := lnValue.Interface().(net.Listener)
	if ok && ln != nil {
		err = ln.Close()
	}

	if svr.listener != nil {
		_ = svr.listener.Close()
	}
	if svr.websocketListener != nil {
		_ = svr.websocketListener.Close()
	}
	if svr.kcpListener != nil {
		_ = svr.kcpListener.Close()
	}
	close(svr.closedCh)
	svr.Closed = true
	svr.rc.TcpPortManager.Stop()
	svr.rc.UdpPortManager.Stop()
	return err
}

func (svr *Service) HandleListener(l net.Listener) {
	defer func() {
		log.Info("Frps is Closed")
	}()
	// Listen for incoming connections from client.
	for {
		log.Info("Wait for new Connect")
		c, err := l.Accept()
		if err != nil {
			log.Warn("Listener for incoming connections from client closed")
			return
		}
		// inject xlog object into net.Conn context
		xl := xlog.New()
		c = frpNet.NewContextConn(c, xlog.NewContext(context.Background(), xl))

		log.Trace("start check TLS connection...")
		originConn := c
		c, err = frpNet.CheckAndEnableTLSServerConnWithTimeout(c, svr.tlsConfig, svr.cfg.TlsOnly, connReadTimeout)
		if err != nil {
			log.Warn("CheckAndEnableTLSServerConnWithTimeout error: %v", err)
			originConn.Close()
			continue
		}
		log.Trace("success check TLS connection")

		// Start a new goroutine for dealing connections.
		go func(frpConn net.Conn) {
			dealFn := func(conn net.Conn) {
				var rawMsg msg.Message
				conn.SetReadDeadline(time.Now().Add(connReadTimeout))
				if rawMsg, err = msg.ReadMsg(conn); err != nil {
					log.Trace("Failed to read message: %v", err)
					conn.Close()
					return
				}
				conn.SetReadDeadline(time.Time{})

				switch m := rawMsg.(type) {
				case *msg.Login:
					// server plugin hook
					content := &plugin.LoginContent{
						Login: *m,
					}
					retContent, err := svr.pluginManager.Login(content)
					if err == nil {
						m = &retContent.Login
						err = svr.RegisterControl(conn, m)
					}

					// If login failed, send error message there.
					// Otherwise send success message in control's work goroutine.
					if err != nil {
						xl.Warn("register control error: %v", err)
						msg.WriteMsg(conn, &msg.LoginResp{
							Version: version.Full(),
							Error:   util.GenerateResponseErrorString("register control error", err, svr.cfg.DetailedErrorsToClient),
						})
						conn.Close()
					}
				case *msg.NewWorkConn:
					if err := svr.RegisterWorkConn(conn, m); err != nil {
						conn.Close()
					}
				case *msg.NewVisitorConn:
					if err = svr.RegisterVisitorConn(conn, m); err != nil {
						xl.Warn("register visitor conn error: %v", err)
						msg.WriteMsg(conn, &msg.NewVisitorConnResp{
							ProxyName: m.ProxyName,
							Error:     util.GenerateResponseErrorString("register visitor conn error", err, svr.cfg.DetailedErrorsToClient),
						})
						conn.Close()
					} else {
						msg.WriteMsg(conn, &msg.NewVisitorConnResp{
							ProxyName: m.ProxyName,
							Error:     "",
						})
					}
				default:
					log.Warn("Error message type for the new connection [%s]", conn.RemoteAddr().String())
					conn.Close()
				}
			}

			if svr.cfg.TcpMux {
				fmuxCfg := fmux.DefaultConfig()
				fmuxCfg.KeepAliveInterval = 20 * time.Second
				fmuxCfg.LogOutput = ioutil.Discard
				session, err := fmux.Server(frpConn, fmuxCfg)
				if err != nil {
					log.Warn("Failed to create mux connection: %v", err)
					frpConn.Close()
					return
				}

				for {
					stream, err := session.AcceptStream()
					if err != nil {
						log.Debug("Accept new mux stream error: %v", err)
						session.Close()
						return
					}
					go dealFn(stream)
				}
			} else {
				dealFn(frpConn)
			}
		}(c)
	}
}

func (svr *Service) RegisterControl(ctlConn net.Conn, loginMsg *msg.Login) (err error) {
	// If client's RunId is empty, it's a new client, we just create a new controller.
	// Otherwise, we check if there is one controller has the same run id. If so, we release previous controller and start new one.
	if loginMsg.RunId == "" {
		loginMsg.RunId, err = util.RandId()
		if err != nil {
			return
		}
	}

	ctx := frpNet.NewContextFromConn(ctlConn)
	xl := xlog.FromContextSafe(ctx)
	xl.AppendPrefix(loginMsg.RunId)
	ctx = xlog.NewContext(ctx, xl)
	xl.Info("client login info: ip [%s] version [%s] hostname [%s] os [%s] arch [%s]",
		ctlConn.RemoteAddr().String(), loginMsg.Version, loginMsg.Hostname, loginMsg.Os, loginMsg.Arch)

	// Check client version.
	if ok, msg := version.Compat(loginMsg.Version); !ok {
		err = fmt.Errorf("%s", msg)
		return
	}

	// Check auth.
	if err = svr.authVerifier.VerifyLogin(loginMsg); err != nil {
		return
	}

	ctl := NewControl(ctx, svr.rc, svr.pxyManager, svr.pluginManager, svr.authVerifier, ctlConn, loginMsg, svr.cfg)
	if oldCtl := svr.ctlManager.Add(loginMsg.RunId, ctl); oldCtl != nil {
		oldCtl.allShutdown.WaitDone()
	}

	ctl.Start()

	// for statistics
	metrics.Server.NewClient()

	go func() {
		// block until control closed
		ctl.WaitClosed()
		svr.ctlManager.Del(loginMsg.RunId, ctl)
	}()
	return
}

// RegisterWorkConn register a new work connection to control and proxies need it.
func (svr *Service) RegisterWorkConn(workConn net.Conn, newMsg *msg.NewWorkConn) error {
	xl := frpNet.NewLogFromConn(workConn)
	ctl, exist := svr.ctlManager.GetById(newMsg.RunId)
	if !exist {
		xl.Warn("No client control found for run id [%s]", newMsg.RunId)
		return fmt.Errorf("no client control found for run id [%s]", newMsg.RunId)
	}
	// Check auth.
	if err := svr.authVerifier.VerifyNewWorkConn(newMsg); err != nil {
		xl.Warn("Invalid authentication in NewWorkConn message on run id [%s]", newMsg.RunId)
		msg.WriteMsg(workConn, &msg.StartWorkConn{
			Error: "invalid authentication in NewWorkConn",
		})
		return fmt.Errorf("invalid authentication in NewWorkConn message on run id [%s]", newMsg.RunId)
	}
	return ctl.RegisterWorkConn(workConn)
}

func (svr *Service) RegisterVisitorConn(visitorConn net.Conn, newMsg *msg.NewVisitorConn) error {
	return svr.rc.VisitorManager.NewConn(newMsg.ProxyName, visitorConn, newMsg.Timestamp, newMsg.SignKey,
		newMsg.UseEncryption, newMsg.UseCompression)
}

// Setup a bare-bones TLS config for the server
func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{tlsCert}}
}
