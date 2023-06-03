/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2023, daeuniverse Organization <team@v2raya.org>
 */

package dae

import (
	"fmt"
	daeConfig "github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/control"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/daeuniverse/dae/pkg/logger"
	"github.com/mohae/deepcopy"
	"github.com/sirupsen/logrus"
	"runtime"
)

type ReloadMessage struct {
	Config   *daeConfig.Config
	Callback chan<- bool
}

var ChReloadConfigs = make(chan *ReloadMessage, 16)
var GracefullyExit = make(chan struct{})
var EmptyConfig *daeConfig.Config

func init() {
	sections, err := config_parser.Parse(`global{} routing{}`)
	if err != nil {
		panic(err)
	}
	EmptyConfig, err = daeConfig.New(sections)
	if err != nil {
		panic(err)
	}
}

func Run(log *logrus.Logger, conf *daeConfig.Config, externGeoDataDirs []string, disableTimestamp bool, dry bool) (err error) {
	defer close(GracefullyExit)
	// Not really run dae.
	if dry {
		log.Infoln("Dry run in api-only mode")
	dryLoop:
		for newConf := range ChReloadConfigs {
			switch newConf {
			case nil:
				break dryLoop
			default:
				newConf.Callback <- true
			}
		}
		return nil
	}

	// New ControlPlane.
	c, err := newControlPlane(log, nil, nil, conf, externGeoDataDirs)
	if err != nil {
		return err
	}

	// Serve tproxy TCP/UDP server util signals.
	var listener *control.Listener
	go func() {
		readyChan := make(chan bool, 1)
		go func() {
			<-readyChan
			log.Infoln("Ready")
		}()
		if listener, err = c.ListenAndServe(readyChan, conf.Global.TproxyPort); err != nil {
			log.Errorln("ListenAndServe:", err)
		}
		// Exit
		ChReloadConfigs <- nil
	}()
	reloading := false
	/* dae-wing start */
	isRollback := false
	var chCallback chan<- bool
	/* dae-wing end */
loop:
	for newReloadMsg := range ChReloadConfigs {
		switch newReloadMsg {
		case nil:
			// We will receive nil after control plane being Closed.
			// We'll judge if we are in a reloading.
			if reloading {
				// Serve.
				reloading = false
				log.Warnln("[Reload] Serve")
				readyChan := make(chan bool, 1)
				go func() {
					if err := c.Serve(readyChan, listener); err != nil {
						log.Errorln("ListenAndServe:", err)
					}
					// Exit
					ChReloadConfigs <- nil
				}()
				<-readyChan
				log.Warnln("[Reload] Finished")
				/* dae-wing start */
				if !isRollback {
					// To notify the success.
					chCallback <- true
				}
				/* dae-wing end */
			} else {
				// Listening error.
				break loop
			}
		default:
			// Reload signal.
			log.Warnln("[Reload] Received reload signal; prepare to reload")

			/* dae-wing start */
			newConf := newReloadMsg.Config
			/* dae-wing end */
			// New logger.
			log = logger.NewLogger(newConf.Global.LogLevel, disableTimestamp)
			logrus.SetLevel(log.Level)

			// New control plane.
			obj := c.EjectBpf()
			var dnsCache map[string]*control.DnsCache
			if conf.Dns.IpVersionPrefer == newConf.Dns.IpVersionPrefer {
				// Only keep dns cache when ip version preference not change.
				dnsCache = c.CloneDnsCache()
			}
			log.Warnln("[Reload] Load new control plane")
			newC, err := newControlPlane(log, obj, dnsCache, newConf, externGeoDataDirs)
			if err != nil {
				log.WithFields(logrus.Fields{
					"err": err,
				}).Errorln("[Reload] Failed to reload; try to roll back configuration")
				// Load last config back.
				newC, err = newControlPlane(log, obj, dnsCache, conf, externGeoDataDirs)
				if err != nil {
					obj.Close()
					c.Close()
					log.WithFields(logrus.Fields{
						"err": err,
					}).Fatalln("[Reload] Failed to roll back configuration")
				}
				newConf = conf
				log.Errorln("[Reload] Last reload failed; rolled back configuration")
			} else {
				log.Warnln("[Reload] Stopped old control plane")
				/* dae-wing start */
				isRollback = false
				/* dae-wing end */
			}

			// Inject bpf objects into the new control plane life-cycle.
			newC.InjectBpf(obj)

			// Prepare new context.
			oldC := c
			c = newC
			conf = newConf
			reloading = true
			/* dae-wing start */
			chCallback = newReloadMsg.Callback
			/* dae-wing end */

			// Ready to close.
			oldC.Close()
		}
	}
	if e := c.Close(); e != nil {
		return fmt.Errorf("close control plane: %w", e)
	}
	return nil
}

func newControlPlane(log *logrus.Logger, bpf interface{}, dnsCache map[string]*control.DnsCache, conf *daeConfig.Config, externGeoDataDirs []string) (c *control.ControlPlane, err error) {

	// Print configuration.
	if log.IsLevelEnabled(logrus.DebugLevel) {
		bConf, _ := conf.Marshal(2)
		log.Debugln(string(bConf))
	}

	// Deep copy to prevent modification.
	conf = deepcopy.Copy(conf).(*daeConfig.Config)

	/// Get subscription -> nodeList mapping.
	subscriptionToNodeList := map[string][]string{}
	if len(conf.Node) > 0 {
		for _, node := range conf.Node {
			subscriptionToNodeList[""] = append(subscriptionToNodeList[""], string(node))
		}
	}
	if len(conf.Subscription) > 0 {
		return nil, fmt.Errorf("daeConfig.subscription is not supported in dae-wing")
	}

	// New dae control plane.
	c, err = control.NewControlPlane(
		log,
		bpf,
		dnsCache,
		subscriptionToNodeList,
		conf.Group,
		&conf.Routing,
		&conf.Global,
		&conf.Dns,
		externGeoDataDirs,
	)
	if err != nil {
		return nil, err
	}
	// Call GC to release memory.
	runtime.GC()

	return c, nil
}
