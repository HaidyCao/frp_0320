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

package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HaidyCao/frp_0320/assets"
	"github.com/HaidyCao/frp_0320/models/auth"
	"github.com/HaidyCao/frp_0320/models/config"
	"github.com/HaidyCao/frp_0320/models/msg"
	"github.com/HaidyCao/frp_0320/utils/log"
	frpNet "github.com/HaidyCao/frp_0320/utils/net"
	"github.com/HaidyCao/frp_0320/utils/version"
	"github.com/HaidyCao/frp_0320/utils/xlog"

	fmux "github.com/hashicorp/yamux"
)

type ServiceClosedListener interface {
	OnClosed(msg string)
}

// Service is a client service.
type Service struct {
	// uniq id got from frps, attach it in loginMsg
	runId string

	// manager control connection with server
	ctl   *Control
	ctlMu sync.RWMutex

	// Sets authentication based on selected method
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	cfgFile string

	// This is configured by the login response from frps
	serverUDPPort int

	exit uint32 // 0 means not exit

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc

	closed           bool
	ReConnectByCount bool
	reConnectCount   int
	onClosedListener ServiceClosedListener
}

func (svr *Service) SetProxyFailedFunc(proxyFailedFunc func(err error)) {
	if svr.ctl != nil {
		svr.ctl.ProxyFunc = proxyFailedFunc
	}
}

func (svr *Service) SetOnCloseListener(listener ServiceClosedListener) {
	svr.onClosedListener = listener
}

func NewService(cfg config.ClientCommonConf, pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf, cfgFile string) (svr *Service, err error) {

	ctx, cancel := context.WithCancel(context.Background())
	svr = &Service{
		authSetter:  auth.NewAuthSetter(cfg.AuthClientConfig),
		cfg:         cfg,
		cfgFile:     cfgFile,
		pxyCfgs:     pxyCfgs,
		visitorCfgs: visitorCfgs,
		exit:        0,
		ctx:         xlog.NewContext(ctx, xlog.New()),
		cancel:      cancel,
	}
	return
}

func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

func (svr *Service) Run(isCmd bool) error {
	xl := xlog.FromContextSafe(svr.ctx)

	// first login
	for {
		conn, session, err := svr.login()
		if err != nil {
			xl.Warn("login to server failed: %v", err)

			// if login_fail_exit is true, just exit this program
			// otherwise sleep a while and try again to connect to server
			if svr.cfg.LoginFailExit {
				return err
			} else {
				time.Sleep(10 * time.Second)
			}
		} else {
			// login success
			ctl := NewControl(svr.ctx, svr.runId, conn, session, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}

	go svr.keepControllerWorking()

	if svr.cfg.AdminPort != 0 {
		// Init admin server assets
		err := assets.Load(svr.cfg.AssetsDir, assets.Frpc)
		if err != nil {
			return fmt.Errorf("Load assets error: %v", err)
		}

		err = svr.RunAdminServer(svr.cfg.AdminAddr, svr.cfg.AdminPort)
		if err != nil {
			log.Warn("run admin server error: %v", err)
		}
		log.Info("admin server listen on %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}

	svr.closed = false
	if isCmd {
		<-svr.ctx.Done()
		svr.closed = true
		log.Info("svr closed")
	} else {
		go func() {
			<-svr.ctx.Done()
			svr.closed = true
			log.Info("svr closed")

			if svr.onClosedListener != nil {
				svr.onClosedListener.OnClosed("")
			}
		}()
	}
	return nil
}

func (svr *Service) keepControllerWorking() {
	xl := xlog.FromContextSafe(svr.ctx)
	maxDelayTime := 20 * time.Second
	delayTime := time.Second

	for {
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		for {
			xl.Info("try to reconnect to server...")
			conn, session, err := svr.login()
			if err != nil {
				xl.Warn("reconnect to server error: %v", err)
				time.Sleep(delayTime)
				delayTime = delayTime * 2
				if delayTime > maxDelayTime {
					delayTime = maxDelayTime
				}
				if svr.ReConnectByCount {
					svr.reConnectCount++
					if svr.reConnectCount == 3 {
						svr.Close()
						return
					}
				}
				continue
			}
			// reconnect success, init delayTime
			delayTime = time.Second

			ctl := NewControl(svr.ctx, svr.runId, conn, session, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}
}

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
func (svr *Service) login() (conn net.Conn, session *fmux.Session, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	var tlsConfig *tls.Config
	if svr.cfg.TLSEnable {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}
	conn, err = frpNet.ConnectServerByProxyWithTLS(svr.cfg.HttpProxy, svr.cfg.Protocol,
		newAddress(svr.cfg.ServerAddr, svr.cfg.ServerPort), tlsConfig)
	if err != nil {
		return
	}

	defer func() {
		if err != nil && conn != nil {
			conn.Close()
			if session != nil {
				session.Close()
			}
		}
	}()

	if svr.cfg.TcpMux {
		fmuxCfg := fmux.DefaultConfig()
		fmuxCfg.KeepAliveInterval = 20 * time.Second
		fmuxCfg.LogOutput = ioutil.Discard
		session, err = fmux.Client(conn, fmuxCfg)
		if err != nil {
			return
		}
		stream, errRet := session.OpenStream()
		if errRet != nil {
			session.Close()
			err = errRet
			return
		}
		conn = stream
	}

	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.cfg.PoolCount,
		User:      svr.cfg.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunId:     svr.runId,
		Metas:     svr.cfg.Metas,
	}

	// Add auth
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		err = fmt.Errorf("%s", loginRespMsg.Error)
		xl.Error("%s", loginRespMsg.Error)
		return
	}

	svr.runId = loginRespMsg.RunId
	xl.ResetPrefixes()
	xl.AppendPrefix(svr.runId)

	svr.serverUDPPort = loginRespMsg.ServerUdpPort
	xl.Info("login to server success, get run id [%s], server udp port [%d]", loginRespMsg.RunId, loginRespMsg.ServerUdpPort)
	return
}

func (svr *Service) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	svr.cfgMu.Lock()
	svr.pxyCfgs = pxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	return svr.ctl.ReloadConf(pxyCfgs, visitorCfgs)
}

func (svr *Service) Close() {
	atomic.StoreUint32(&svr.exit, 1)
	if svr.ctl != nil {
		_ = svr.ctl.Close()
	}
	svr.cancel()
	svr.ctl = nil
}

func (svr *Service) IsClosed() bool {
	return svr.closed
}
