package core

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/aler9/gortsplib/pkg/base"
	"github.com/aler9/gortsplib/pkg/headers"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
)

type pathManagerParent interface {
	Log(logger.Level, string, ...interface{})
}

type pathManager struct {
	rtspAddress     string
	readTimeout     time.Duration
	writeTimeout    time.Duration
	readBufferCount int
	readBufferSize  int
	authMethods     []headers.AuthMethod
	pathConfs       map[string]*conf.PathConf
	stats           *stats
	parent          pathManagerParent

	ctx       context.Context
	ctxCancel func()
	wg        sync.WaitGroup
	paths     map[string]*path

	// in
	confReload  chan map[string]*conf.PathConf
	pathClose   chan *path
	rpDescribe  chan readPublisherDescribeReq
	rpSetupPlay chan readPublisherSetupPlayReq
	rpAnnounce  chan readPublisherAnnounceReq
}

func newPathManager(
	parentCtx context.Context,
	rtspAddress string,
	readTimeout time.Duration,
	writeTimeout time.Duration,
	readBufferCount int,
	readBufferSize int,
	authMethods []headers.AuthMethod,
	pathConfs map[string]*conf.PathConf,
	stats *stats,
	parent pathManagerParent) *pathManager {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	pm := &pathManager{
		rtspAddress:     rtspAddress,
		readTimeout:     readTimeout,
		writeTimeout:    writeTimeout,
		readBufferCount: readBufferCount,
		readBufferSize:  readBufferSize,
		authMethods:     authMethods,
		pathConfs:       pathConfs,
		stats:           stats,
		parent:          parent,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
		paths:           make(map[string]*path),
		confReload:      make(chan map[string]*conf.PathConf),
		pathClose:       make(chan *path),
		rpDescribe:      make(chan readPublisherDescribeReq),
		rpSetupPlay:     make(chan readPublisherSetupPlayReq),
		rpAnnounce:      make(chan readPublisherAnnounceReq),
	}

	pm.createPaths()

	pm.wg.Add(1)
	go pm.run()

	return pm
}

func (pm *pathManager) close() {
	pm.ctxCancel()
	pm.wg.Wait()
}

// Log is the main logging function.
func (pm *pathManager) Log(level logger.Level, format string, args ...interface{}) {
	pm.parent.Log(level, format, args...)
}

func (pm *pathManager) run() {
	defer pm.wg.Done()

outer:
	for {
		select {
		case pathConfs := <-pm.confReload:
			// remove confs
			for pathName := range pm.pathConfs {
				if _, ok := pathConfs[pathName]; !ok {
					delete(pm.pathConfs, pathName)
				}
			}

			// update confs
			for pathName, oldConf := range pm.pathConfs {
				if !oldConf.Equal(pathConfs[pathName]) {
					pm.pathConfs[pathName] = pathConfs[pathName]
				}
			}

			// add confs
			for pathName, pathConf := range pathConfs {
				if _, ok := pm.pathConfs[pathName]; !ok {
					pm.pathConfs[pathName] = pathConf
				}
			}

			// remove paths associated with a conf which doesn't exist anymore
			// or has changed
			for _, pa := range pm.paths {
				if pathConf, ok := pm.pathConfs[pa.ConfName()]; !ok || pathConf != pa.Conf() {
					delete(pm.paths, pa.Name())
					pa.Close()
				}
			}

			// add paths
			pm.createPaths()

		case pa := <-pm.pathClose:
			if pmpa, ok := pm.paths[pa.Name()]; !ok || pmpa != pa {
				continue
			}
			delete(pm.paths, pa.Name())
			pa.Close()

		case req := <-pm.rpDescribe:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readPublisherDescribeRes{Err: err}
				continue
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.ReadIPsParsed,
				pathConf.ReadUser,
				pathConf.ReadPass,
			)
			if err != nil {
				req.Res <- readPublisherDescribeRes{Err: err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.PathName]; !ok {
				pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[req.PathName].OnPathManDescribe(req)

		case req := <-pm.rpSetupPlay:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readPublisherSetupPlayRes{Err: err}
				continue
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.ReadIPsParsed,
				pathConf.ReadUser,
				pathConf.ReadPass,
			)
			if err != nil {
				req.Res <- readPublisherSetupPlayRes{Err: err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.PathName]; !ok {
				pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[req.PathName].OnPathManSetupPlay(req)

		case req := <-pm.rpAnnounce:
			pathName, pathConf, err := pm.findPathConf(req.PathName)
			if err != nil {
				req.Res <- readPublisherAnnounceRes{Err: err}
				continue
			}

			err = pm.authenticate(
				req.IP,
				req.ValidateCredentials,
				req.PathName,
				pathConf.PublishIPsParsed,
				pathConf.PublishUser,
				pathConf.PublishPass,
			)
			if err != nil {
				req.Res <- readPublisherAnnounceRes{Err: err}
				continue
			}

			// create path if it doesn't exist
			if _, ok := pm.paths[req.PathName]; !ok {
				pm.createPath(pathName, pathConf, req.PathName)
			}

			pm.paths[req.PathName].OnPathManAnnounce(req)

		case <-pm.ctx.Done():
			break outer
		}
	}

	pm.ctxCancel()
}

func (pm *pathManager) createPath(confName string, conf *conf.PathConf, name string) {
	pm.paths[name] = newPath(
		pm.ctx,
		pm.rtspAddress,
		pm.readTimeout,
		pm.writeTimeout,
		pm.readBufferCount,
		pm.readBufferSize,
		confName,
		conf,
		name,
		&pm.wg,
		pm.stats,
		pm)
}

func (pm *pathManager) createPaths() {
	for pathName, pathConf := range pm.pathConfs {
		if _, ok := pm.paths[pathName]; !ok && pathConf.Regexp == nil {
			pm.createPath(pathName, pathConf, pathName)
		}
	}
}

func (pm *pathManager) findPathConf(name string) (string, *conf.PathConf, error) {
	err := conf.CheckPathName(name)
	if err != nil {
		return "", nil, fmt.Errorf("invalid path name: %s (%s)", err, name)
	}

	// normal path
	if pathConf, ok := pm.pathConfs[name]; ok {
		return name, pathConf, nil
	}

	// regular expression path
	for pathName, pathConf := range pm.pathConfs {
		if pathConf.Regexp != nil && pathConf.Regexp.MatchString(name) {
			return pathName, pathConf, nil
		}
	}

	return "", nil, fmt.Errorf("unable to find a valid configuration for path '%s'", name)
}

// OnConfReload is called by core.
func (pm *pathManager) OnConfReload(pathConfs map[string]*conf.PathConf) {
	select {
	case pm.confReload <- pathConfs:
	case <-pm.ctx.Done():
	}
}

// OnPathClose is called by path.
func (pm *pathManager) OnPathClose(pa *path) {
	select {
	case pm.pathClose <- pa:
	case <-pm.ctx.Done():
	}
}

// OnReadPublisherDescribe is called by a ReadPublisher.
func (pm *pathManager) OnReadPublisherDescribe(req readPublisherDescribeReq) {
	select {
	case pm.rpDescribe <- req:
	case <-pm.ctx.Done():
		req.Res <- readPublisherDescribeRes{Err: fmt.Errorf("terminated")}
	}
}

// OnReadPublisherAnnounce is called by a ReadPublisher.
func (pm *pathManager) OnReadPublisherAnnounce(req readPublisherAnnounceReq) {
	select {
	case pm.rpAnnounce <- req:
	case <-pm.ctx.Done():
		req.Res <- readPublisherAnnounceRes{Err: fmt.Errorf("terminated")}
	}
}

// OnReadPublisherSetupPlay is called by a ReadPublisher.
func (pm *pathManager) OnReadPublisherSetupPlay(req readPublisherSetupPlayReq) {
	select {
	case pm.rpSetupPlay <- req:
	case <-pm.ctx.Done():
		req.Res <- readPublisherSetupPlayRes{Err: fmt.Errorf("terminated")}
	}
}

func (pm *pathManager) authenticate(
	ip net.IP,
	validateCredentials func(authMethods []headers.AuthMethod, pathUser string, pathPass string) error,
	pathName string,
	pathIPs []interface{},
	pathUser string,
	pathPass string,
) error {
	// validate ip
	if pathIPs != nil && ip != nil {
		if !ipEqualOrInRange(ip, pathIPs) {
			return readPublisherErrAuthCritical{
				Message: fmt.Sprintf("IP '%s' not allowed", ip),
				Response: &base.Response{
					StatusCode: base.StatusUnauthorized,
				},
			}
		}
	}

	// validate user
	if pathUser != "" && validateCredentials != nil {
		err := validateCredentials(pm.authMethods, pathUser, pathPass)
		if err != nil {
			return err
		}
	}

	return nil
}
